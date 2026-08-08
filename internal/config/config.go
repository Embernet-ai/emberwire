// Package config loads Emberwire's runtime configuration.
//
// Node-RED's settings.js is executable JavaScript: adminAuth can be a function,
// https can be a function returning cert options, storageModule is a require().
// That makes it impossible to validate, diff, template from a ConfigMap, or
// reason about without running it. Emberwire's configuration is declarative
// YAML with environment overrides, which is what a Helm chart can actually
// produce and what an operator can actually review.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// Config is the whole configuration surface.
type Config struct {
	Server    Server    `yaml:"server"`
	Data      Data      `yaml:"data"`
	Auth      Auth      `yaml:"auth"`
	Runtime   Runtime   `yaml:"runtime"`
	Discovery Discovery `yaml:"discovery"`
	Exec      Exec      `yaml:"exec"`
	Files     Files     `yaml:"files"`
	Logging   Logging   `yaml:"logging"`
	Metrics   Metrics   `yaml:"metrics"`
}

// Server is the HTTP listener.
type Server struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	// AdminRoot is the path prefix the editor and admin API are served under.
	// The App Store proxies apps by path, so this has to be settable.
	AdminRoot string `yaml:"adminRoot"`

	ReadTimeout     time.Duration `yaml:"readTimeout"`
	WriteTimeout    time.Duration `yaml:"writeTimeout"`
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`

	// MaxRequestBytes bounds a flow deploy. A flow file is the largest thing
	// the API accepts and an unbounded body is a trivial way to OOM an edge box.
	MaxRequestBytes int64 `yaml:"maxRequestBytes"`
}

// Data is where state lives. All of it under one directory, which is the PVC.
type Data struct {
	Dir             string `yaml:"dir"`
	FlowFile        string `yaml:"flowFile"`
	CredentialsFile string `yaml:"credentialsFile"`

	// CredentialSecret encrypts the credential store. Leaving it empty writes
	// credentials in plaintext, which is refused unless AllowPlaintextCredentials
	// is set — see Validate.
	CredentialSecret          string `yaml:"credentialSecret"`
	AllowPlaintextCredentials bool   `yaml:"allowPlaintextCredentials"`
	BackupGenerations         int    `yaml:"backupGenerations"`
}

// Auth controls access to the editor and admin API.
type Auth struct {
	// Enabled defaults to true. Starting without it requires an explicit
	// opt-out, because Node-RED shipping unauthenticated by default is the root
	// cause of CVE-2025-41656 — unauthenticated RCE by deploying a flow with an
	// exec node in it. Node-RED's own team proposed exactly this fix in
	// designs#81 and did not ship it.
	Enabled bool `yaml:"enabled"`

	Users []User `yaml:"users"`

	// SessionTTL bounds how long an issued token is good for.
	SessionTTL time.Duration `yaml:"sessionTTL"`
}

// User is a local account.
type User struct {
	Username string `yaml:"username"`
	// PasswordHash is a bcrypt hash. Plaintext passwords are refused, so a
	// ConfigMap can never contain one by accident.
	PasswordHash string `yaml:"passwordHash"`
	// Permissions is "*" for full access, or a list such as ["flows.read"].
	Permissions []string `yaml:"permissions"`
}

// Runtime tunes the scheduler.
type Runtime struct {
	InboxCapacity int           `yaml:"inboxCapacity"`
	Overflow      string        `yaml:"overflow"`
	BlockTimeout  time.Duration `yaml:"blockTimeout"`
	CloseTimeout  time.Duration `yaml:"closeTimeout"`
}

// Discovery gates the network discovery nodes.
type Discovery struct {
	Enabled bool `yaml:"enabled"`
	// AllowedCIDRs bounds what the scan nodes may probe. This is a plant-floor
	// inventory tool, not a scanner somebody can aim anywhere from an edit
	// dialog, so an empty list with discovery enabled is a configuration error
	// rather than "scan everything".
	AllowedCIDRs []string `yaml:"allowedCIDRs"`
}

// Exec gates the exec node.
//
// Off by default, and an enabled node with no allowed commands is a
// configuration error rather than "anything goes". Node-RED's exec node against
// a default configuration is CVE-2025-41656 — unauthenticated remote code
// execution, reached by deploying a flow — and the reason it is that severe is
// that nothing between "can edit a flow" and "can run any command" exists. This
// is that thing.
type Exec struct {
	Enabled bool `yaml:"enabled"`
	// AllowedCommands lists what a flow may run. Entries are matched on the
	// resolved absolute path, so naming "curl" allows the curl on the PATH at
	// the time and not whatever a later mount puts in front of it.
	AllowedCommands []string `yaml:"allowedCommands"`
}

// Files bounds where the file nodes may read and write.
//
// Unlike the exec node this is on by default, scoped to the data directory. A
// flow reading and writing under its own PVC is the ordinary case; reaching
// outside it is not, and Node-RED's file nodes taking any path is what makes
// "can edit a flow" mean "can read any file this process can".
type Files struct {
	// AllowedPaths are extra directory trees on top of the data directory.
	AllowedPaths []string `yaml:"allowedPaths"`
}

// Logging controls the runtime log.
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // text or json
}

// Metrics controls the Prometheus endpoint.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// Default returns the configuration used when nothing is specified.
func Default() Config {
	return Config{
		Server: Server{
			Host:      "0.0.0.0",
			Port:      1880,
			AdminRoot: "/",
			// Generous read timeout: a deploy of a large flow over a slow edge
			// link is legitimate. Write timeout is zero because the comms
			// websocket is long-lived and a deadline would cut it.
			ReadTimeout:     60 * time.Second,
			WriteTimeout:    0,
			ShutdownTimeout: 20 * time.Second,
			MaxRequestBytes: 32 << 20, // 32 MiB
		},
		Data: Data{
			Dir:               "/data",
			FlowFile:          "flows.json",
			CredentialsFile:   "credentials.json",
			BackupGenerations: 3,
		},
		Auth: Auth{
			Enabled:    true,
			SessionTTL: 7 * 24 * time.Hour,
		},
		Runtime: Runtime{
			InboxCapacity: 1024,
			Overflow:      "block",
			BlockTimeout:  30 * time.Second,
			CloseTimeout:  15 * time.Second,
		},
		Logging: Logging{Level: "info", Format: "text"},
		Metrics: Metrics{Enabled: true, Path: "/metrics"},
	}
}

// Load reads a configuration file, applies environment overrides and validates
// the result. An empty path uses the defaults plus the environment.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("reading %s: %w", path, err)
		}
		// KnownFields makes a typo in a ConfigMap a startup failure instead of
		// a setting that silently does nothing. An operator who misspells
		// "credentialSecret" should find out immediately, not when they
		// discover the credential file is plaintext.
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parsing %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv overlays EMBERWIRE_* environment variables.
//
// Environment beats file so that a Helm chart can put non-secret settings in a
// ConfigMap and inject the secret ones from a Secret, which is the only sane
// split in Kubernetes.
func applyEnv(cfg *Config) {
	envStr("EMBERWIRE_HOST", &cfg.Server.Host)
	envInt("EMBERWIRE_PORT", &cfg.Server.Port)
	envStr("EMBERWIRE_ADMIN_ROOT", &cfg.Server.AdminRoot)

	envStr("EMBERWIRE_DATA_DIR", &cfg.Data.Dir)
	envStr("EMBERWIRE_FLOW_FILE", &cfg.Data.FlowFile)
	envStr("EMBERWIRE_CREDENTIAL_SECRET", &cfg.Data.CredentialSecret)

	envInt("EMBERWIRE_INBOX_CAPACITY", &cfg.Runtime.InboxCapacity)
	envStr("EMBERWIRE_OVERFLOW", &cfg.Runtime.Overflow)

	envStr("EMBERWIRE_LOG_LEVEL", &cfg.Logging.Level)
	envStr("EMBERWIRE_LOG_FORMAT", &cfg.Logging.Format)

	// A single admin account can be supplied entirely from the environment,
	// which is what makes a first-run container usable without mounting a file.
	user := os.Getenv("EMBERWIRE_ADMIN_USER")
	hash := os.Getenv("EMBERWIRE_ADMIN_PASSWORD_HASH")
	if user != "" && hash != "" {
		cfg.Auth.Users = append(cfg.Auth.Users, User{
			Username:     user,
			PasswordHash: hash,
			Permissions:  []string{"*"},
		})
	}

	if envBool("EMBERWIRE_INSECURE") {
		cfg.Auth.Enabled = false
	}
	if envBool("EMBERWIRE_ALLOW_PLAINTEXT_CREDENTIALS") {
		cfg.Data.AllowPlaintextCredentials = true
	}
	if envBool("EMBERWIRE_DISCOVERY_ENABLED") {
		cfg.Discovery.Enabled = true
	}
	if v := os.Getenv("EMBERWIRE_DISCOVERY_CIDRS"); v != "" {
		cfg.Discovery.AllowedCIDRs = splitList(v)
	}
	if envBool("EMBERWIRE_EXEC_ENABLED") {
		cfg.Exec.Enabled = true
	}
	if v := os.Getenv("EMBERWIRE_EXEC_ALLOWED_COMMANDS"); v != "" {
		cfg.Exec.AllowedCommands = splitList(v)
	}
	if v := os.Getenv("EMBERWIRE_FILE_ALLOWED_PATHS"); v != "" {
		cfg.Files.AllowedPaths = splitList(v)
	}
}

func envStr(key string, dst *string) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		*dst = v
	}
}

func envInt(key string, dst *int) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// envBool treats only the explicit affirmatives as true, so EMBERWIRE_INSECURE=0
// or =false does not disable authentication by accident.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ErrInsecure is returned when authentication is disabled without the explicit
// opt-out. It is a distinct type so main can print the remedy rather than a
// bare error.
type ErrInsecure struct{ Reason string }

func (e *ErrInsecure) Error() string {
	return "refusing to start: " + e.Reason
}

// Validate checks the configuration and refuses the dangerous combinations.
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is not a valid port", c.Server.Port)
	}
	if !strings.HasPrefix(c.Server.AdminRoot, "/") {
		return fmt.Errorf("server.adminRoot %q must start with /", c.Server.AdminRoot)
	}
	if c.Data.Dir == "" {
		return fmt.Errorf("data.dir is empty")
	}
	if c.Data.BackupGenerations < 0 {
		return fmt.Errorf("data.backupGenerations must not be negative")
	}

	switch c.Runtime.Overflow {
	case "block", "drop-newest", "drop-oldest", "error":
	default:
		return fmt.Errorf("runtime.overflow %q is not one of block, drop-newest, drop-oldest, error",
			c.Runtime.Overflow)
	}
	if c.Runtime.InboxCapacity < 1 {
		return fmt.Errorf("runtime.inboxCapacity must be at least 1")
	}

	switch c.Logging.Level {
	case "error", "warn", "info", "debug", "trace":
	default:
		return fmt.Errorf("logging.level %q is not one of error, warn, info, debug, trace", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format %q is not text or json", c.Logging.Format)
	}

	// Authentication. This is the check that exists because Node-RED does not
	// have it: CVE-2025-41656 is unauthenticated RCE against a default config,
	// achieved by deploying a flow containing an exec node.
	if !c.Auth.Enabled {
		return &ErrInsecure{Reason: "authentication is disabled. " +
			"Anyone who can reach this port can deploy a flow, and a flow can run commands. " +
			"Set EMBERWIRE_INSECURE=true to override this on a trusted, isolated network."}
	}
	if len(c.Auth.Users) == 0 {
		return &ErrInsecure{Reason: "authentication is enabled but no users are configured. " +
			"Set auth.users in the config file, or EMBERWIRE_ADMIN_USER and " +
			"EMBERWIRE_ADMIN_PASSWORD_HASH in the environment. " +
			"Generate a hash with: emberwire hash-password"}
	}
	for i, u := range c.Auth.Users {
		if u.Username == "" {
			return fmt.Errorf("auth.users[%d] has no username", i)
		}
		if u.PasswordHash == "" {
			return fmt.Errorf("auth.users[%d] (%s) has no passwordHash", i, u.Username)
		}
		// Refuse anything that is not a bcrypt hash, so a plaintext password
		// cannot end up in a ConfigMap by accident. bcrypt.Cost parses the
		// prefix and fails on anything else.
		if _, err := bcrypt.Cost([]byte(u.PasswordHash)); err != nil {
			return fmt.Errorf("auth.users[%d] (%s): passwordHash is not a bcrypt hash. "+
				"Generate one with: emberwire hash-password", i, u.Username)
		}
	}

	// Credentials at rest.
	if c.Data.CredentialSecret == "" && !c.Data.AllowPlaintextCredentials {
		return &ErrInsecure{Reason: "no credential secret is set, so node credentials " +
			"would be written to disk in plaintext. " +
			"Set data.credentialSecret or EMBERWIRE_CREDENTIAL_SECRET, " +
			"or set EMBERWIRE_ALLOW_PLAINTEXT_CREDENTIALS=true if this instance holds no secrets."}
	}

	// Discovery.
	if c.Discovery.Enabled && len(c.Discovery.AllowedCIDRs) == 0 {
		return fmt.Errorf("discovery.enabled is true but discovery.allowedCIDRs is empty; " +
			"list the networks the scan nodes may probe, or disable discovery")
	}

	// The exec node. Same shape as discovery and for the same reason: an empty
	// allowlist read permissively is how a narrow capability becomes a shell.
	if c.Exec.Enabled && len(c.Exec.AllowedCommands) == 0 {
		return fmt.Errorf("exec.enabled is true but exec.allowedCommands is empty; " +
			"list the commands a flow may run, or disable the exec node")
	}

	return nil
}

// FlowPath is the absolute path to the flow file.
func (c *Config) FlowPath() string { return filepath.Join(c.Data.Dir, c.Data.FlowFile) }

// CredentialsPath is the absolute path to the credential store.
func (c *Config) CredentialsPath() string {
	return filepath.Join(c.Data.Dir, c.Data.CredentialsFile)
}

// Addr is the listen address.
func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port) }

// FindUser returns a configured user by name.
func (c *Config) FindUser(username string) (User, bool) {
	for _, u := range c.Auth.Users {
		if u.Username == username {
			return u, true
		}
	}
	return User{}, false
}

// Allows reports whether a user holds a permission. "*" grants everything.
func (u User) Allows(perm string) bool {
	for _, p := range u.Permissions {
		if p == "*" || p == perm {
			return true
		}
		// A prefix grant such as "flows.*" covers "flows.write".
		if strings.HasSuffix(p, ".*") && strings.HasPrefix(perm, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// HashPassword produces a bcrypt hash for the hash-password command.
func HashPassword(plain string) (string, error) {
	if len(plain) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword verifies a plaintext password against a stored hash in constant
// time.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
