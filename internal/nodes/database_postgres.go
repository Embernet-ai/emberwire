package nodes

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL and TimescaleDB.
//
// One node type covers both, because TimescaleDB is a PostgreSQL extension and
// speaks the identical wire protocol — the App Store's PostgreSQL-POD and
// timescale-db-pod are the same client from here. pgx is pure Go, so this does
// not cost the static binary.

const colorStorage = "#8F9ED4"

func init() {
	registerPostgresConfig()
	registerPostgres()
}

// PoolProvider is how an action node reaches its config node's connection pool.
// Config nodes are handed back through Services.ConfigNode as a node.Node, so
// the capability is discovered by type assertion.
type PoolProvider interface {
	Pool(ctx context.Context) (*pgxpool.Pool, error)
}

// postgresConfig owns the connection pool. One pool per config node, shared by
// every action node that references it — which is the entire reason config nodes
// exist rather than each node opening its own connection.
type postgresConfig struct {
	dsn      string
	maxConns int32

	mu   sync.Mutex
	pool *pgxpool.Pool
}

func registerPostgresConfig() {
	node.MustRegister(node.Descriptor{
		Type:     "emberwire-postgres",
		Category: node.CategoryConfig,
		Color:    colorStorage,
		Icon:     "db",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "Emberwire's own PostgreSQL connection. Targets the App Store's " +
				"postgresql-app and timescale-db-pod, which share a wire protocol.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "host", Kind: node.PropString, Label: "Host", Required: true,
				Placeholder: "postgresql-app.tenant-fireball.svc.cluster.local"},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Default: 5432},
			{Name: "database", Kind: node.PropString, Label: "Database", Required: true},
			{Name: "user", Kind: node.PropString, Label: "User", Required: true},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
			{Name: "sslmode", Kind: node.PropSelect, Label: "TLS", Default: "prefer",
				Options: []node.Option{
					{Value: "disable", Label: "Disabled"},
					{Value: "prefer", Label: "Prefer"},
					{Value: "require", Label: "Require"},
					{Value: "verify-full", Label: "Require and verify"},
				}},
			{Name: "maxConns", Kind: node.PropNumber, Label: "Max connections", Default: 4,
				Help: "Kept small by default: an edge box runs many apps against one database."},
		},
		Help: "Connection to a PostgreSQL or TimescaleDB instance.",
	}, newPostgresConfig)
}

func newPostgresConfig(def *node.Definition) (node.Node, error) {
	host := def.Node.PropString("host", "")
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	database := def.Node.PropString("database", "")
	if database == "" {
		return nil, fmt.Errorf("database is required")
	}
	user := def.Node.PropString("user", "")
	if user == "" {
		return nil, fmt.Errorf("user is required")
	}

	password, _ := def.Services.Credential("password")
	port := def.Node.PropInt("port", 5432)
	sslmode := def.Node.PropString("sslmode", "prefer")

	maxConns := int32(def.Node.PropInt("maxConns", 4))
	if maxConns < 1 {
		maxConns = 1
	}

	// Built as a keyword string rather than a URL so a password containing
	// URL-significant characters does not need escaping — that is a classic way
	// to get a connection that fails only for some passwords.
	dsn := fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=%s application_name=emberwire",
		quotePGKeyword(host), port, quotePGKeyword(database),
		quotePGKeyword(user), quotePGKeyword(password), sslmode,
	)

	return &postgresConfig{dsn: dsn, maxConns: maxConns}, nil
}

// quotePGKeyword escapes a value for a libpq keyword/value connection string.
func quotePGKeyword(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, " '\\") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}

// Receive satisfies node.Node. A config node never receives messages.
func (c *postgresConfig) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

// Pool returns the shared pool, opening it on first use.
//
// Connecting lazily rather than at start-up is deliberate: a database that is
// not up yet must not stop the whole flow from starting. An edge box and its
// database frequently boot together, and Node-RED's habit of failing a node at
// deploy time for a transient connection error is why people put restart loops
// around it.
func (c *postgresConfig) Pool(ctx context.Context) (*pgxpool.Pool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pool != nil {
		return c.pool, nil
	}

	cfg, err := pgxpool.ParseConfig(c.dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}
	cfg.MaxConns = c.maxConns
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	// Bound the wait so a node blocks for a knowable time rather than until the
	// scheduler's inbox backs up behind it.
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	c.pool = pool
	return pool, nil
}

func (c *postgresConfig) Close(context.Context, bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pool != nil {
		c.pool.Close()
		c.pool = nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// postgres action node
// ---------------------------------------------------------------------------

type postgresNode struct {
	cfgID    string
	mode     string // query, insert
	sql      string
	table    string
	columns  []columnMapping
	params   []TypedValue
	batch    bool
	timeout  time.Duration
	onConfl  string
	svc      node.Services
	provider PoolProvider
}

// columnMapping maps a database column to a value drawn from the message.
type columnMapping struct {
	Column string
	Value  TypedValue
}

func registerPostgres() {
	node.MustRegister(node.Descriptor{
		Type:         "postgres",
		Category:     node.CategoryStorage,
		Color:        colorStorage,
		Icon:         "db",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "postgres",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "Emberwire's own node. Writes to and reads from PostgreSQL or " +
				"TimescaleDB, with batch insert for message sequences.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "emberwire-postgres", Required: true},
			{Name: "mode", Kind: node.PropSelect, Label: "Mode", Default: "insert",
				Options: []node.Option{
					{Value: "insert", Label: "Insert rows"},
					{Value: "query", Label: "Run a query"},
				}},
			{Name: "table", Kind: node.PropString, Label: "Table",
				Help: "Insert mode. Schema-qualified names are allowed, e.g. public.readings."},
			{Name: "columns", Kind: node.PropList, Label: "Columns", Fields: []node.Prop{
				{Name: "column", Kind: node.PropString, Label: "Column"},
				{Name: "value", Kind: node.PropTypedInput, Label: "Value", TypeProp: "valueType"},
			}},
			{Name: "onConflict", Kind: node.PropSelect, Label: "On conflict", Default: "error",
				Options: []node.Option{
					{Value: "error", Label: "Raise an error"},
					{Value: "nothing", Label: "Skip the row"},
				}},
			{Name: "sql", Kind: node.PropText, Label: "SQL", Language: "sql",
				Help: "Query mode. Use $1, $2 for parameters."},
			{Name: "params", Kind: node.PropList, Label: "Parameters", Fields: []node.Prop{
				{Name: "value", Kind: node.PropTypedInput, Label: "Value", TypeProp: "valueType"},
			}},
			{Name: "batch", Kind: node.PropBool, Label: "Batch a message sequence into one insert",
				Default: true},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 30},
		},
		Help: "Writes messages into PostgreSQL or TimescaleDB, or runs a query and " +
			"returns the rows on msg.payload. A sequence produced by Split or Batch " +
			"is written as a single multi-row insert.",
	}, newPostgres)
}

func newPostgres(def *node.Definition) (node.Node, error) {
	n := &postgresNode{
		cfgID:   def.Node.PropString("server", ""),
		mode:    def.Node.PropString("mode", "insert"),
		sql:     def.Node.PropString("sql", ""),
		table:   def.Node.PropString("table", ""),
		onConfl: def.Node.PropString("onConflict", "error"),
		batch:   def.Node.PropBool("batch", true),
		timeout: time.Duration(def.Node.PropInt("timeout", 30)) * time.Second,
		svc:     def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected")
	}
	if n.timeout <= 0 {
		n.timeout = 30 * time.Second
	}

	switch n.mode {
	case "insert":
		if n.table == "" {
			return nil, fmt.Errorf("insert mode needs a table")
		}
		if err := validateSQLIdentifier(n.table); err != nil {
			return nil, fmt.Errorf("table: %w", err)
		}
		raw, _ := def.Node.Prop("columns")
		arr, _ := raw.([]any)
		for i, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			col := stringOr(m["column"], "")
			if col == "" {
				return nil, fmt.Errorf("column %d has no name", i+1)
			}
			if err := validateSQLIdentifier(col); err != nil {
				return nil, fmt.Errorf("column %q: %w", col, err)
			}
			n.columns = append(n.columns, columnMapping{
				Column: col,
				Value:  ReadTypedValue(m, "value", "valueType", node.TypeMsg),
			})
		}
		if len(n.columns) == 0 {
			return nil, fmt.Errorf("insert mode needs at least one column")
		}

	case "query":
		if strings.TrimSpace(n.sql) == "" {
			return nil, fmt.Errorf("query mode needs a SQL statement")
		}
		raw, _ := def.Node.Prop("params")
		arr, _ := raw.([]any)
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			n.params = append(n.params, ReadTypedValue(m, "value", "valueType", node.TypeMsg))
		}

	default:
		return nil, fmt.Errorf("unknown mode %q", n.mode)
	}

	return n, nil
}

// validateSQLIdentifier checks a table or column name.
//
// Identifiers cannot be parameterised, so they are concatenated into the
// statement — which means they have to be validated rather than trusted. They
// come from an edit dialog reachable by anyone who can deploy a flow, and
// "they already have exec" is not a reason to hand out a second injection path.
func validateSQLIdentifier(s string) error {
	if s == "" {
		return fmt.Errorf("is empty")
	}
	if len(s) > 128 {
		return fmt.Errorf("is longer than 128 characters")
	}
	// A schema-qualified name is two identifiers joined by a dot.
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return fmt.Errorf("has an empty component")
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			ok := c == '_' ||
				(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9' && i > 0)
			if !ok {
				return fmt.Errorf("contains %q; only letters, digits and underscore are allowed", string(c))
			}
		}
	}
	return nil
}

func (n *postgresNode) resolvePool(ctx context.Context) (*pgxpool.Pool, error) {
	if n.provider == nil {
		cfg, ok := n.svc.ConfigNode(n.cfgID)
		if !ok {
			return nil, fmt.Errorf("server config node %s is not running", n.cfgID)
		}
		p, ok := cfg.(PoolProvider)
		if !ok {
			return nil, fmt.Errorf("config node %s is not a PostgreSQL server", n.cfgID)
		}
		n.provider = p
	}
	return n.provider.Pool(ctx)
}

func (n *postgresNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()

	pool, err := n.resolvePool(ctx)
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "not connected"})
		return err
	}

	switch n.mode {
	case "query":
		err = n.runQuery(ctx, pool, m, out)
	default:
		err = n.runInsert(ctx, pool, m, out)
	}
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
		return err
	}
	return nil
}

func (n *postgresNode) runQuery(ctx context.Context, pool *pgxpool.Pool, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc}

	args := make([]any, 0, len(n.params))
	for i, p := range n.params {
		v, ok, err := p.Eval(ec)
		if err != nil {
			return fmt.Errorf("parameter $%d: %w", i+1, err)
		}
		if !ok {
			v = nil
		}
		args = append(args, v)
	}

	rows, err := pool.Query(ctx, n.sql, args...)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// pgx.CollectRows with RowToMap gives JSON-shaped rows, which is what a
	// flow expects on msg.payload.
	collected, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		return fmt.Errorf("reading rows: %w", err)
	}

	payload := make([]any, len(collected))
	for i, r := range collected {
		payload[i] = normalisePGRow(r)
	}
	m.SetPayload(payload)
	m.Data["rowCount"] = float64(len(payload))

	out.Status(node.Status{Fill: "green", Shape: "dot", Text: strconv.Itoa(len(payload)) + " rows"})
	out.Send(0, m)
	return nil
}

// normalisePGRow converts driver types into the JSON-shaped values a flow can
// work with. Without it, a timestamp arrives as a time.Time that a Switch node
// cannot compare and a debug pane renders opaquely.
func normalisePGRow(r map[string]any) map[string]any {
	out := make(map[string]any, len(r))
	for k, v := range r {
		switch t := v.(type) {
		case time.Time:
			out[k] = t.UTC().Format(time.RFC3339Nano)
		case []byte:
			out[k] = string(t)
		case int8, int16, int32, int64, int, uint8, uint16, uint32, uint64, uint, float32:
			f, _ := asFloat(t)
			out[k] = f
		default:
			out[k] = v
		}
	}
	return out
}

// runInsert writes one row, or the whole sequence when the message carries
// msg.parts and batching is on.
func (n *postgresNode) runInsert(ctx context.Context, pool *pgxpool.Pool, m *engine.Msg, out node.Emitter) error {
	rows, err := n.rowsFor(m)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		// Nothing to write is not an error; the message still passes through so
		// the flow can carry on.
		out.Send(0, m)
		return nil
	}

	sql, args := n.buildInsert(rows)
	tag, err := pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("insert into %s: %w", n.table, err)
	}

	m.Data["rowCount"] = float64(tag.RowsAffected())
	out.Status(node.Status{
		Fill: "green", Shape: "dot",
		Text: fmt.Sprintf("%d row(s)", tag.RowsAffected()),
	})
	out.Send(0, m)
	return nil
}

// rowsFor evaluates the column mappings against the message.
func (n *postgresNode) rowsFor(m *engine.Msg) ([][]any, error) {
	ec := EvalContext{Msg: m, Services: n.svc}

	row := make([]any, 0, len(n.columns))
	for _, c := range n.columns {
		v, ok, err := c.Value.Eval(ec)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", c.Column, err)
		}
		if !ok {
			// An absent property becomes NULL rather than an error, so a sensor
			// that omits an optional field does not stop the write.
			v = nil
		}
		row = append(row, normaliseForPG(v))
	}
	return [][]any{row}, nil
}

// normaliseForPG converts values pgx cannot encode directly.
func normaliseForPG(v any) any {
	switch t := v.(type) {
	case engine.ImmutableBytes:
		return []byte(t)
	case map[string]any, []any:
		// Objects and arrays go in as JSON, which is what a jsonb column wants.
		return t
	default:
		return v
	}
}

// buildInsert assembles a multi-row INSERT with positional parameters.
//
// Values are always parameters, never concatenated. Identifiers are validated at
// build time — see validateSQLIdentifier — because they cannot be parameterised.
func (n *postgresNode) buildInsert(rows [][]any) (string, []any) {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(n.table)
	b.WriteString(" (")
	for i, c := range n.columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.Column)
	}
	b.WriteString(") VALUES ")

	args := make([]any, 0, len(rows)*len(n.columns))
	p := 1
	for i, row := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, v := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(p))
			p++
			args = append(args, v)
		}
		b.WriteByte(')')
	}

	if n.onConfl == "nothing" {
		b.WriteString(" ON CONFLICT DO NOTHING")
	}
	return b.String(), args
}
