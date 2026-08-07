// Command emberwire runs the flow engine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/embernet-ai/emberwire/internal/api"
	"github.com/embernet-ai/emberwire/internal/config"
	"github.com/embernet-ai/emberwire/internal/discover"
	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
	"github.com/embernet-ai/emberwire/internal/nodes" // registers the built-in palette
	"github.com/embernet-ai/emberwire/internal/runtime"
	"github.com/embernet-ai/emberwire/internal/shell"
	"github.com/embernet-ai/emberwire/internal/store"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		// A configuration refusal gets printed plainly with its remedy rather
		// than as a stack of wrapped errors. The operator reading this is
		// probably looking at a CrashLoopBackOff.
		var insecure *config.ErrInsecure
		if errors.As(err, &insecure) {
			fmt.Fprintf(os.Stderr, "\nemberwire %s\n\n%s\n\n", version, insecure.Error())
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "emberwire: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "hash-password":
			return cmdHashPassword(os.Args[2:])
		case "import":
			return cmdImport(os.Args[2:])
		case "version":
			fmt.Println(version)
			return nil
		default:
			return fmt.Errorf("unknown command %q (try: serve, hash-password, import, version)", os.Args[1])
		}
	}
	return cmdServe(os.Args[1:])
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("emberwire", flag.ContinueOnError)
	configPath := fs.String("config", os.Getenv("EMBERWIRE_CONFIG"), "path to the YAML configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log := newLogger(cfg.Logging)
	log.Info("starting", "version", version, "addr", cfg.Addr(), "dataDir", cfg.Data.Dir)

	// Install the discovery scope before any flow starts, so a scan node can
	// never run against an unbounded scope even for one message.
	scope, err := discover.NewScope(cfg.Discovery.Enabled, cfg.Discovery.AllowedCIDRs)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	nodes.Scope = scope
	if cfg.Discovery.Enabled {
		log.Warn("network discovery is enabled",
			"allowedCIDRs", strings.Join(cfg.Discovery.AllowedCIDRs, ","))
	}

	// Same treatment for the exec node: the policy is installed before any flow
	// starts, so a node can never run for even one message against an unset one.
	commands, err := shell.NewPolicy(cfg.Exec.Enabled, cfg.Exec.AllowedCommands)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	nodes.Commands = commands
	if cfg.Exec.Enabled {
		log.Warn("the exec node is enabled",
			"allowedCommands", strings.Join(commands.Allowed(), ","))
		// Said at boot rather than at the first message: an operator who
		// misspelled a command should find out while they are still looking.
		if missing := commands.Unresolved(); len(missing) > 0 {
			log.Warn("some allowed commands were not found on the PATH; they will be "+
				"resolved again if a flow uses one, in case they are mounted later",
				"commands", strings.Join(missing, ","))
		}
	}

	if err := os.MkdirAll(cfg.Data.Dir, 0o700); err != nil {
		return fmt.Errorf("creating data directory %s: %w", cfg.Data.Dir, err)
	}

	flowStore := store.NewFlowStore(cfg.FlowPath())
	flowStore.SetBackupGenerations(cfg.Data.BackupGenerations)

	creds := store.NewCredentialStore(cfg.CredentialsPath(), cfg.Data.CredentialSecret)
	if err := creds.Load(); err != nil {
		return fmt.Errorf("loading credentials: %w", err)
	}
	if creds.MigratedFromLegacy() {
		// Worth saying out loud: the operator should know their credentials
		// arrived in Node-RED's format and are about to be re-encrypted.
		log.Warn("credentials were read in Node-RED's AES-256-CTR format and will be " +
			"re-encrypted with AES-256-GCM on the next deploy")
	}
	if !creds.HasSecret() {
		log.Warn("no credential secret is set; node credentials are stored in plaintext")
	}

	app := &application{
		cfg:       cfg,
		log:       log,
		flowStore: flowStore,
		creds:     creds,
		registry:  node.Default,
		contexts:  store.NewScopedContexts(),
	}

	srv := api.New(api.Deps{
		Config:      cfg,
		Registry:    node.Default,
		Flows:       flowStore,
		Credentials: creds,
		Logger:      log,
		Runtime:     app.currentRuntime,
		Deploy:      app.deploy,
		Version:     version,
	})
	app.hub = srv.Hub()

	// Load and start whatever is on disk before opening the port, so the first
	// request never races the runtime coming up.
	flows, err := flowStore.Load()
	if err != nil {
		return fmt.Errorf("loading flows: %w", err)
	}
	if recovered, from := flowStore.Recovered(); recovered {
		log.Error("the flow file was unparseable and was recovered from a backup",
			"backup", from, "corruptFileKept", cfg.FlowPath()+".corrupt")
	}
	for _, w := range flows.Warnings {
		log.Warn("flow warning", "detail", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Per-node failures are logged inside start and are not fatal: a flow with
	// one bad node still runs the other nodes, and refusing to boot over a
	// single typo would take a line down for no reason.
	app.start(ctx, flows)

	httpServer := &http.Server{
		Addr:         cfg.Addr(),
		Handler:      srv.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr(), "adminRoot", cfg.Server.AdminRoot)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Stop accepting requests first, then stop the flows. The other order would
	// let a deploy land against a runtime that is halfway through stopping.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown did not complete cleanly", "error", err)
	}
	app.stop(shutdownCtx)
	log.Info("stopped")
	return nil
}

// ---------------------------------------------------------------------------
// application
// ---------------------------------------------------------------------------

// application owns the running runtime and the deploy cycle.
type application struct {
	cfg       config.Config
	log       *slog.Logger
	flowStore *store.FlowStore
	creds     *store.CredentialStore
	registry  *node.Registry
	contexts  *store.ScopedContexts
	hub       interface{ Broadcast(runtime.Event) }

	mu      sync.Mutex
	rt      *runtime.Runtime
	pumpCtx context.CancelFunc
}

func (a *application) currentRuntime() *runtime.Runtime {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rt
}

func (a *application) runtimeOptions() runtime.Options {
	return runtime.Options{
		InboxCapacity: a.cfg.Runtime.InboxCapacity,
		Overflow:      runtime.OverflowPolicy(a.cfg.Runtime.Overflow),
		BlockTimeout:  a.cfg.Runtime.BlockTimeout,
		CloseTimeout:  a.cfg.Runtime.CloseTimeout,
	}
}

// start builds a runtime for a flow set and starts it, returning the per-node
// failures it tolerated. A non-empty result does not mean the start failed —
// one node with a bad config or an unknown type must not take the deploy down.
func (a *application) start(ctx context.Context, flows *engine.Flows) []runtime.StartError {
	rt := runtime.New(a.registry, flows, a.runtimeOptions())
	rt.SetContexts(a.contexts)
	rt.SetCredentials(func(nodeID string) map[string]string {
		return a.creds.Get(nodeID)
	})

	// Pump runtime events into the websocket hub before starting, so nothing
	// emitted during start-up is lost.
	pumpCtx, cancel := context.WithCancel(ctx)
	go a.pump(pumpCtx, rt)

	failures := rt.Start(ctx)
	for _, f := range failures {
		a.log.Error("node failed to start", "node", f.NodeID, "type", f.Type, "error", f.Err)
	}

	a.mu.Lock()
	a.rt = rt
	a.pumpCtx = cancel
	a.mu.Unlock()

	a.log.Info("flows started",
		"nodes", len(flows.Nodes), "tabs", len(flows.Tabs), "failures", len(failures))
	return failures
}

// pump forwards runtime events to connected editors.
func (a *application) pump(ctx context.Context, rt *runtime.Runtime) {
	events := rt.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if a.hub != nil {
				a.hub.Broadcast(e)
			}
		}
	}
}

func (a *application) stop(ctx context.Context) {
	a.mu.Lock()
	rt, cancel := a.rt, a.pumpCtx
	a.rt = nil
	a.mu.Unlock()

	if rt != nil {
		for _, err := range rt.Stop(ctx) {
			a.log.Warn("error stopping a node", "error", err)
		}
	}
	if cancel != nil {
		cancel()
	}
}

// deploy replaces the running flows.
//
// The order matters and is not the obvious one. Credentials are split out and
// the flow file is written *before* the old runtime is stopped, so that a
// failure to persist leaves the previous flows running rather than taking the
// line down for a bad save.
func (a *application) deploy(ctx context.Context, flows *engine.Flows, expectedRev string) (api.DeployResult, error) {
	incoming := flows.StripCredentials()
	for id, c := range incoming {
		a.creds.Merge(id, c)
	}

	live := make(map[string]bool, len(flows.Nodes))
	for id := range flows.Nodes {
		live[id] = true
	}
	if removed := a.creds.Prune(live); removed > 0 {
		a.log.Info("pruned credentials for deleted nodes", "count", removed)
	}

	rev, err := a.flowStore.Save(flows, expectedRev)
	if err != nil {
		return api.DeployResult{}, err
	}
	if err := a.creds.Save(); err != nil {
		return api.DeployResult{}, fmt.Errorf("saving credentials: %w", err)
	}

	a.stop(ctx)

	// Drop context belonging to nodes and flows that no longer exist, so
	// redeploying repeatedly does not accumulate state for things that are gone.
	liveFlows := make(map[string]bool, len(flows.Tabs)+len(flows.Subflows))
	for id := range flows.Tabs {
		liveFlows[id] = true
	}
	for id := range flows.Subflows {
		liveFlows[id] = true
	}
	a.contexts.Clean(live, liveFlows)

	// Start against the background context, not the request's: the request is
	// about to complete and its cancellation must not tear down the flows.
	failures := a.start(context.Background(), flows)

	a.log.Info("deployed", "rev", rev, "failures", len(failures))
	return api.DeployResult{
		Rev:      rev,
		Warnings: flows.Warnings,
		Failures: failures,
	}, nil
}

// ---------------------------------------------------------------------------
// hash-password
// ---------------------------------------------------------------------------

func cmdHashPassword(args []string) error {
	fs := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	pass := fs.String("password", "", "password to hash (omit to read from EMBERWIRE_PASSWORD)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	plain := *pass
	if plain == "" {
		plain = os.Getenv("EMBERWIRE_PASSWORD")
	}
	if plain == "" {
		return errors.New("no password given: pass -password, or set EMBERWIRE_PASSWORD")
	}

	hash, err := config.HashPassword(plain)
	if err != nil {
		return err
	}
	fmt.Println(hash)
	return nil
}

// ---------------------------------------------------------------------------
// import
// ---------------------------------------------------------------------------

// cmdImport reports what would happen to a Node-RED flow file before anyone
// deploys it, which is the difference between finding out now and finding out
// when a line stops.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: emberwire import <flows.json>")
	}

	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	flows, err := engine.ParseFlows(data)
	if err != nil {
		return err
	}

	supported := map[string]int{}
	unsupported := map[string]int{}
	partial := map[string]string{}

	for _, id := range flows.Order {
		n, ok := flows.Nodes[id]
		if !ok {
			continue
		}
		if _, isInstance := n.SubflowTemplateID(); isInstance {
			supported["subflow instance"]++
			continue
		}
		reg, known := node.Default.Lookup(n.Type)
		if !known {
			unsupported[n.Type]++
			continue
		}
		supported[n.Type]++
		if reg.Descriptor.Compatibility.Level != node.CompatFull {
			partial[n.Type] = reg.Descriptor.Compatibility.Notes
		}
	}

	fmt.Printf("%s\n\n", fs.Arg(0))
	fmt.Printf("  %d entries: %d nodes, %d tabs, %d subflows, %d groups\n\n",
		len(flows.Order), len(flows.Nodes), len(flows.Tabs), len(flows.Subflows), len(flows.Groups))

	if len(flows.Warnings) > 0 {
		fmt.Printf("Warnings\n")
		for _, w := range flows.Warnings {
			fmt.Printf("  - %s\n", w)
		}
		fmt.Println()
	}

	fmt.Printf("Supported node types\n")
	for _, t := range sortedCounts(supported) {
		fmt.Printf("  %-32s %d\n", t.name, t.count)
	}
	fmt.Println()

	if len(partial) > 0 {
		fmt.Printf("Partially supported — read these before deploying\n")
		for t, notes := range partial {
			fmt.Printf("  %s\n      %s\n", t, notes)
		}
		fmt.Println()
	}

	if len(unsupported) > 0 {
		fmt.Printf("NOT supported — these nodes will not start\n")
		for _, t := range sortedCounts(unsupported) {
			fmt.Printf("  %-32s %d\n", t.name, t.count)
		}
		fmt.Printf("\nThe rest of the flow still runs. See docs/compatibility.md.\n")
		return nil
	}

	fmt.Printf("Every node type in this flow is implemented.\n")
	return nil
}

type nameCount struct {
	name  string
	count int
}

func sortedCounts(m map[string]int) []nameCount {
	out := make([]nameCount, 0, len(m))
	for k, v := range m {
		out = append(out, nameCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].name < out[j].name
	})
	return out
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

func newLogger(cfg config.Logging) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "error":
		level = slog.LevelError
	case "warn":
		level = slog.LevelWarn
	case "debug", "trace":
		level = slog.LevelDebug
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
