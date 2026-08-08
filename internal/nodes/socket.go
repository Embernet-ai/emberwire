package nodes

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerTCPIn()
	registerTCPOut()
	registerTCPRequest()
	registerUDPIn()
	registerUDPOut()
}

// Bounds on the socket nodes. Node-RED has none of these: a peer that opens
// connections and never closes them, or sends a megabyte with no delimiter in
// it, grows the heap until the pod dies.
const (
	// maxSocketConnections is how many peers one TCP In or TCP Out server will
	// hold open.
	maxSocketConnections = 256

	// maxSocketFrameBytes bounds one delimited message. Past it the connection
	// is closed, because a peer that has sent this much without a delimiter is
	// not speaking the protocol the node was configured for.
	maxSocketFrameBytes = 8 << 20

	// socketReconnectDelay is how long a client waits before dialling again.
	socketReconnectDelay = 3 * time.Second

	// defaultSocketTimeout bounds a TCP Request.
	defaultSocketTimeout = 10 * time.Second

	// maxDatagramBytes is the read buffer for a UDP datagram. 65507 is the
	// largest an IPv4 datagram can carry.
	maxDatagramBytes = 65507
)

// ---------------------------------------------------------------------------
// payload encoding, shared by the TCP and UDP nodes
// ---------------------------------------------------------------------------

// socketPayload renders received bytes according to the node's datatype.
func socketPayload(raw []byte, datatype string) (any, error) {
	switch datatype {
	case "", "buffer":
		// ImmutableBytes: raw is freshly allocated per read and never written
		// to again, so fanning it out shares rather than copies.
		return engine.ImmutableBytes(raw), nil
	case "utf8", "string":
		return string(raw), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(raw), nil
	}
	return nil, fmt.Errorf("unknown data type %q", datatype)
}

// socketBytes turns a payload into bytes to put on the wire.
func socketBytes(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(t), nil
	case []byte:
		return t, nil
	case engine.ImmutableBytes:
		return t, nil
	case []any:
		// Node-RED writes an array of byte values as a Buffer, which is how a
		// flow constructs a Modbus or EtherNet/IP frame in a Function node.
		out := make([]byte, 0, len(t))
		for i, e := range t {
			f, ok := asFloat(e)
			if !ok || f < 0 || f > 255 {
				return nil, fmt.Errorf("element %d of the payload is %v, not a byte", i, e)
			}
			out = append(out, byte(f))
		}
		return out, nil
	default:
		return []byte(mustacheString(v)), nil
	}
}

// unescapeDelimiter expands the two-character escapes Node-RED's edit dialog
// stores, so a newline delimiter typed as \n is one byte rather than two.
var delimiterUnescaper = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\0`, "\x00")

func unescapeDelimiter(s string) string { return delimiterUnescaper.Replace(s) }

// splitOnDelimiter builds a bufio.SplitFunc for a multi-byte delimiter.
//
// bufio.ScanLines only handles newlines, and a plant-floor protocol is as
// likely to terminate on ETX or a null as on one.
func splitOnDelimiter(delim []byte) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (int, []byte, error) {
		if i := indexBytes(data, delim); i >= 0 {
			return i + len(delim), data[:i], nil
		}
		if atEOF && len(data) > 0 {
			// Whatever arrived before the connection closed is still data. The
			// alternative is discarding the last message of every stream that
			// does not end with its delimiter.
			return len(data), data, nil
		}
		return 0, nil, nil
	}
}

func indexBytes(haystack, needle []byte) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// tcp sessions
// ---------------------------------------------------------------------------

// tcpSession is one open connection, tracked so a TCP Out node can reply to the
// peer a message came from.
type tcpSession struct {
	id   string
	conn net.Conn

	mu     sync.Mutex
	closed bool
}

func (s *tcpSession) write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("the connection to %s is closed", s.conn.RemoteAddr())
	}
	_, err := s.conn.Write(data)
	return err
}

func (s *tcpSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.conn.Close()
}

// tcpSessions is the shared bookkeeping for a listener or a client.
type tcpSessions struct {
	mu   sync.RWMutex
	open map[string]*tcpSession
}

func newTCPSessions() *tcpSessions {
	return &tcpSessions{open: map[string]*tcpSession{}}
}

func (t *tcpSessions) add(conn net.Conn) (*tcpSession, error) {
	s := &tcpSession{id: engine.GenerateID(), conn: conn}

	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.open) >= maxSocketConnections {
		return nil, fmt.Errorf("already holding %d connections", maxSocketConnections)
	}
	t.open[s.id] = s
	return s, nil
}

func (t *tcpSessions) remove(s *tcpSession) {
	t.mu.Lock()
	delete(t.open, s.id)
	t.mu.Unlock()
	s.close()
}

func (t *tcpSessions) lookup(id string) (*tcpSession, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.open[id]
	return s, ok
}

func (t *tcpSessions) all() []*tcpSession {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*tcpSession, 0, len(t.open))
	for _, s := range t.open {
		out = append(out, s)
	}
	return out
}

func (t *tcpSessions) closeAll() {
	for _, s := range t.all() {
		t.remove(s)
	}
}

func (t *tcpSessions) count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.open)
}

// ---------------------------------------------------------------------------
// tcp in
// ---------------------------------------------------------------------------

type tcpInNode struct {
	mode      string // server, client
	host      string
	port      int
	datamode  string // stream, single
	datatype  string
	delimiter []byte
	topic     string
	trim      bool

	sessions *tcpSessions
	listener net.Listener
	mu       sync.Mutex
}

func registerTCPIn() {
	node.MustRegister(node.Descriptor{
		Type:         "tcp in",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "bridge",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "tcp in",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Listens or connects out, in stream mode with a delimiter or single " +
				"mode collecting until the peer closes, with buffer, string or base64 " +
				"payloads. Every message carries msg._session so a TCP Out node can " +
				"reply on the same connection. Three bounds Node-RED does not have: " +
				"the number of accepted connections, the size of one delimited message, " +
				"and the total of a single-mode read. A peer that opens connections " +
				"and never closes them, or sends without ever sending the delimiter, " +
				"grows the heap until the pod dies otherwise. TLS is not implemented.",
			UnsupportedProps: []string{"tls"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropSelect, Label: "Mode", Default: "server",
				Options: []node.Option{
					{Value: "server", Label: "Listen on a port"},
					{Value: "client", Label: "Connect to a host"},
				}},
			{Name: "host", Kind: node.PropString, Label: "Host",
				Help: "Client mode only. Leave empty to listen on every interface."},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Required: true},
			{Name: "datamode", Kind: node.PropSelect, Label: "Deliver", Default: "stream",
				Options: []node.Option{
					{Value: "stream", Label: "A message per delimiter"},
					{Value: "single", Label: "One message when the connection closes"},
				}},
			{Name: "datatype", Kind: node.PropSelect, Label: "Payload", Default: "buffer",
				Options: []node.Option{
					{Value: "buffer", Label: "Raw bytes"},
					{Value: "utf8", Label: "A string"},
					{Value: "base64", Label: "base64"},
				}},
			{Name: "newline", Kind: node.PropString, Label: "Delimiter", Default: `\n`},
			{Name: "topic", Kind: node.PropString, Label: "Topic"},
			{Name: "trim", Kind: node.PropBool, Label: "Trim the delimiter off the payload",
				Default: true},
		},
		Help: "Receives over TCP, either as a server or by connecting out. In stream " +
			"mode each delimiter ends a message; in single mode the whole connection " +
			"becomes one.",
	}, newTCPIn)
}

func newTCPIn(def *node.Definition) (node.Node, error) {
	n := &tcpInNode{
		mode:     def.Node.PropString("server", "server"),
		host:     strings.TrimSpace(def.Node.PropString("host", "")),
		port:     def.Node.PropInt("port", 0),
		datamode: def.Node.PropString("datamode", "stream"),
		datatype: orDefault(def.Node.PropString("datatype", ""), "buffer"),
		topic:    def.Node.PropString("topic", ""),
		trim:     def.Node.PropBool("trim", true),
		sessions: newTCPSessions(),
	}
	if err := validatePort(n.port); err != nil {
		return nil, err
	}
	switch n.mode {
	case "server", "client":
	default:
		return nil, fmt.Errorf("unknown mode %q", n.mode)
	}
	switch n.datamode {
	case "stream", "single":
	default:
		return nil, fmt.Errorf("unknown delivery mode %q", n.datamode)
	}
	if _, err := socketPayload(nil, n.datatype); err != nil {
		return nil, err
	}
	if n.mode == "client" && n.host == "" {
		return nil, fmt.Errorf("client mode needs a host to connect to")
	}

	n.delimiter = []byte(unescapeDelimiter(def.Node.PropString("newline", `\n`)))
	if n.datamode == "stream" && len(n.delimiter) == 0 {
		return nil, fmt.Errorf("stream mode needs a delimiter; use single mode to read " +
			"until the connection closes instead")
	}

	// A TCP Out node in reply mode has to find the connection a message arrived
	// on, and the two nodes may be on different tabs with nothing in the flow
	// graph connecting them — the same problem the Link nodes have.
	TCPReplies.register(n.sessions)
	return n, nil
}

func (n *tcpInNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *tcpInNode) Start(ctx context.Context, out node.Emitter) error {
	if n.mode == "client" {
		go n.dialLoop(ctx, out)
		return nil
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(n.host, strconv.Itoa(n.port)))
	if err != nil {
		return fmt.Errorf("listening on port %d: %w", n.port, err)
	}
	n.mu.Lock()
	n.listener = ln
	n.mu.Unlock()

	out.Status(node.Status{Fill: "green", Shape: "ring", Text: "listening on " + ln.Addr().String()})

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		n.sessions.closeAll()
	}()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := n.sessions.add(conn)
			if err != nil {
				// Refused loudly rather than accepted and forgotten. A peer
				// that never closes its connections would otherwise exhaust the
				// process's file descriptors and take the editor with it.
				_ = conn.Close()
				out.Error(fmt.Errorf("refused a connection from %s: %w", conn.RemoteAddr(), err), nil)
				continue
			}
			out.Status(node.Status{Fill: "green", Shape: "dot",
				Text: fmt.Sprintf("%d connection(s)", n.sessions.count())})
			go n.readConn(ctx, s, out)
		}
	}()
	return nil
}

func (n *tcpInNode) dialLoop(ctx context.Context, out node.Emitter) {
	addr := net.JoinHostPort(n.host, strconv.Itoa(n.port))
	var d net.Dialer

	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			out.Status(node.Status{Fill: "green", Shape: "dot", Text: "connected"})
			if s, addErr := n.sessions.add(conn); addErr == nil {
				n.readConn(ctx, s, out)
			} else {
				_ = conn.Close()
			}
			out.Status(node.Status{Fill: "red", Shape: "ring", Text: "disconnected"})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(socketReconnectDelay):
		}
	}
}

// readConn pumps one connection until it closes.
func (n *tcpInNode) readConn(ctx context.Context, s *tcpSession, out node.Emitter) {
	defer n.sessions.remove(s)

	go func() {
		<-ctx.Done()
		s.close()
	}()

	if n.datamode == "single" {
		raw, err := io.ReadAll(io.LimitReader(s.conn, maxSocketFrameBytes+1))
		if len(raw) > maxSocketFrameBytes {
			out.Error(fmt.Errorf("%s sent more than the %d byte limit without closing",
				s.conn.RemoteAddr(), maxSocketFrameBytes), nil)
			return
		}
		if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			out.Error(fmt.Errorf("reading from %s: %w", s.conn.RemoteAddr(), err), nil)
		}
		if len(raw) > 0 {
			n.emit(raw, s, out)
		}
		return
	}

	sc := bufio.NewScanner(s.conn)
	sc.Buffer(make([]byte, 0, 64<<10), maxSocketFrameBytes)
	sc.Split(splitOnDelimiter(n.delimiter))

	for sc.Scan() {
		frame := sc.Bytes()
		// Copied: the scanner reuses its buffer, and the message outlives this
		// iteration.
		raw := make([]byte, len(frame))
		copy(raw, frame)
		if !n.trim {
			raw = append(raw, n.delimiter...)
		}
		n.emit(raw, s, out)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		if errors.Is(err, bufio.ErrTooLong) {
			out.Error(fmt.Errorf("%s sent more than %d bytes without the delimiter; "+
				"the connection was closed", s.conn.RemoteAddr(), maxSocketFrameBytes), nil)
			return
		}
		out.Error(fmt.Errorf("reading from %s: %w", s.conn.RemoteAddr(), err), nil)
	}
}

func (n *tcpInNode) emit(raw []byte, s *tcpSession, out node.Emitter) {
	payload, err := socketPayload(raw, n.datatype)
	if err != nil {
		out.Error(err, nil)
		return
	}
	m := engine.NewMsgWithPayload(payload)
	if n.topic != "" {
		m.SetTopic(n.topic)
	}
	host, port, _ := net.SplitHostPort(s.conn.RemoteAddr().String())
	m.Data["ip"] = host
	if p, convErr := strconv.Atoi(port); convErr == nil {
		m.Data["port"] = float64(p)
	}
	m.Data["_session"] = map[string]any{"type": "tcp", "id": s.id}
	out.Send(0, m)
}

func (n *tcpInNode) Close(context.Context, bool) error {
	n.mu.Lock()
	ln := n.listener
	n.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	n.sessions.closeAll()
	return nil
}

// ---------------------------------------------------------------------------
// tcp out
// ---------------------------------------------------------------------------

type tcpOutNode struct {
	mode   string // client, reply
	host   string
	port   int
	end    bool
	base64 bool

	// resolve finds the session a reply belongs to. Set from the TCP In node
	// registry, because a reply has to go back down the connection the request
	// arrived on and that connection belongs to the In node.
	mu      sync.Mutex
	conn    net.Conn
	dialing bool
}

// tcpReplyRegistry maps a session id to the connection it belongs to, so a TCP
// Out node in reply mode can find the peer a TCP In node accepted.
//
// Process-wide for the same reason the link registry is: the two nodes may be on
// different tabs, and there is nothing in the flow graph connecting them.
type tcpReplyRegistry struct {
	mu   sync.RWMutex
	sets []*tcpSessions
}

// TCPReplies is the process-wide reply registry.
var TCPReplies = &tcpReplyRegistry{}

func (r *tcpReplyRegistry) register(s *tcpSessions) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets = append(r.sets, s)
}

// Reset clears the registry. Called when a runtime stops, so a session set
// belonging to a flow that is gone cannot be found by the next one.
func (r *tcpReplyRegistry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets = nil
}

func (r *tcpReplyRegistry) lookup(id string) (*tcpSession, bool) {
	r.mu.RLock()
	sets := append([]*tcpSessions(nil), r.sets...)
	r.mu.RUnlock()

	for _, set := range sets {
		if s, ok := set.lookup(id); ok {
			return s, true
		}
	}
	return nil, false
}

func registerTCPOut() {
	node.MustRegister(node.Descriptor{
		Type:         "tcp out",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "bridge",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "tcp out",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Connects to a host and sends, or replies on the connection a TCP In " +
				"node accepted, found through msg._session. Listening for inbound " +
				"connections purely to write to them, which Node-RED's third mode does, " +
				"is not implemented — use a TCP In node for the listening half and this " +
				"node in reply mode. TLS is not implemented.",
			UnsupportedProps: []string{"tls"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "beserver", Kind: node.PropSelect, Label: "Mode", Default: "client",
				Options: []node.Option{
					{Value: "client", Label: "Connect to a host and send"},
					{Value: "reply", Label: "Reply to the TCP In connection"},
				}},
			{Name: "host", Kind: node.PropString, Label: "Host"},
			{Name: "port", Kind: node.PropNumber, Label: "Port"},
			{Name: "end", Kind: node.PropBool, Label: "Close the connection after sending"},
			{Name: "base64", Kind: node.PropBool, Label: "Decode the payload from base64"},
		},
		Help: "Sends the payload over TCP. In reply mode the message must still " +
			"carry msg._session from the TCP In node that received it.",
	}, newTCPOut)
}

func newTCPOut(def *node.Definition) (node.Node, error) {
	n := &tcpOutNode{
		mode:   def.Node.PropString("beserver", "client"),
		host:   strings.TrimSpace(def.Node.PropString("host", "")),
		port:   def.Node.PropInt("port", 0),
		end:    def.Node.PropBool("end", false),
		base64: def.Node.PropBool("base64", false),
	}
	switch n.mode {
	case "client":
		if n.host == "" {
			return nil, fmt.Errorf("no host configured")
		}
		if err := validatePort(n.port); err != nil {
			return nil, err
		}
	case "reply":
	default:
		return nil, fmt.Errorf("unknown mode %q; this build supports client and reply", n.mode)
	}
	return n, nil
}

func (n *tcpOutNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	data, err := n.payloadBytes(m)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}

	if n.mode == "reply" {
		sess, ok := m.Data["_session"].(map[string]any)
		if !ok {
			return fmt.Errorf("the message carries no msg._session, so there is no " +
				"connection to reply on; it must come from a tcp in node")
		}
		id, _ := sess["id"].(string)
		s, found := TCPReplies.lookup(id)
		if !found {
			return fmt.Errorf("the tcp session %s is no longer connected", id)
		}
		if err := s.write(data); err != nil {
			return err
		}
		if n.end {
			s.close()
		}
		return nil
	}

	conn, err := n.connection(ctx)
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "disconnected"})
		return err
	}
	if _, err := conn.Write(data); err != nil {
		n.dropConnection()
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "write failed"})
		return fmt.Errorf("writing to %s:%d: %w", n.host, n.port, err)
	}
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "connected"})
	if n.end {
		n.dropConnection()
	}
	return nil
}

func (n *tcpOutNode) payloadBytes(m *engine.Msg) ([]byte, error) {
	if n.base64 {
		s, ok := m.Payload().(string)
		if !ok {
			return nil, fmt.Errorf("the node is set to decode base64 but the payload is %T",
				m.Payload())
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("the payload is not valid base64: %w", err)
		}
		return raw, nil
	}
	return socketBytes(m.Payload())
}

// connection returns the open connection, dialling if there is not one.
//
// The connection is kept between messages, which is what makes this usable in
// front of a device that counts connections — plenty of PLCs allow four.
func (n *tcpOutNode) connection(ctx context.Context) (net.Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn != nil {
		return n.conn, nil
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(n.host, strconv.Itoa(n.port)))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s:%d: %w", n.host, n.port, err)
	}
	n.conn = conn
	return conn, nil
}

func (n *tcpOutNode) dropConnection() {
	n.mu.Lock()
	conn := n.conn
	n.conn = nil
	n.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (n *tcpOutNode) Close(context.Context, bool) error {
	n.dropConnection()
	return nil
}

// ---------------------------------------------------------------------------
// tcp request
// ---------------------------------------------------------------------------

type tcpRequestNode struct {
	host     string
	port     int
	out      string // time, char, count, sit, immed
	splitc   string
	timeout  time.Duration
	datatype string
	svc      node.Services
}

func registerTCPRequest() {
	node.MustRegister(node.Descriptor{
		Type:         "tcp request",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "bridge",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "tcp request",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Connects, sends the payload and waits for the reply, in any of " +
				"Node-RED's four wait modes: a fixed time, a delimiter, a byte count, " +
				"or until the peer closes. msg.host and msg.port override the node. " +
				"Every request opens its own connection — Node-RED's connection-reuse " +
				"mode is not implemented, and reusing one would change the semantics " +
				"of the wait modes, which all end at a connection boundary. TLS is not " +
				"implemented.",
			UnsupportedProps: []string{"tls"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "host", Kind: node.PropString, Label: "Host"},
			{Name: "port", Kind: node.PropNumber, Label: "Port"},
			{Name: "out", Kind: node.PropSelect, Label: "Return", Default: "time",
				Options: []node.Option{
					{Value: "time", Label: "Whatever arrives within a time"},
					{Value: "char", Label: "Everything up to a delimiter"},
					{Value: "count", Label: "A fixed number of bytes"},
					{Value: "sit", Label: "Everything until the peer closes"},
					{Value: "immed", Label: "Nothing; send and close"},
				}},
			{Name: "splitc", Kind: node.PropString, Label: "Delimiter or count", Default: "0"},
			{Name: "datatype", Kind: node.PropSelect, Label: "Payload", Default: "buffer",
				Options: []node.Option{
					{Value: "buffer", Label: "Raw bytes"},
					{Value: "utf8", Label: "A string"},
					{Value: "base64", Label: "base64"},
				}},
			{Name: "ew_timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 10},
		},
		Help: "Sends the payload to a host and returns the reply. The wait mode " +
			"decides when the reply is considered complete.",
	}, newTCPRequest)
}

func newTCPRequest(def *node.Definition) (node.Node, error) {
	n := &tcpRequestNode{
		host:     strings.TrimSpace(def.Node.PropString("host", "")),
		port:     def.Node.PropInt("port", 0),
		out:      def.Node.PropString("out", "time"),
		splitc:   def.Node.PropString("splitc", "0"),
		timeout:  defaultSocketTimeout,
		datatype: orDefault(def.Node.PropString("datatype", ""), "buffer"),
		svc:      def.Services,
	}
	switch n.out {
	case "time", "char", "count", "sit", "immed":
	default:
		return nil, fmt.Errorf("unknown wait mode %q", n.out)
	}
	if _, err := socketPayload(nil, n.datatype); err != nil {
		return nil, err
	}
	if secs := def.Node.PropFloat("ew_timeout", 0); secs > 0 {
		n.timeout = time.Duration(secs * float64(time.Second))
	}

	switch n.out {
	case "count":
		count, err := strconv.Atoi(strings.TrimSpace(n.splitc))
		if err != nil || count <= 0 {
			return nil, fmt.Errorf("a byte count is needed for this wait mode, got %q", n.splitc)
		}
	case "char":
		if unescapeDelimiter(n.splitc) == "" {
			return nil, fmt.Errorf("a delimiter is needed for this wait mode")
		}
	case "time":
		if ms, err := strconv.Atoi(strings.TrimSpace(n.splitc)); err == nil && ms > 0 {
			n.timeout = time.Duration(ms) * time.Millisecond
		}
	}
	return n, nil
}

func (n *tcpRequestNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	host := n.host
	if v, ok := m.Data["host"].(string); ok && v != "" {
		host = v
	}
	port := n.port
	if f, ok := asFloat(m.Data["port"]); ok && f > 0 {
		port = int(f)
	}
	if host == "" {
		return fmt.Errorf("no host configured and msg.host is not set")
	}
	if err := validatePort(port); err != nil {
		return err
	}

	data, err := socketBytes(m.Payload())
	if err != nil {
		return err
	}

	dialCtx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "failed"})
		return fmt.Errorf("connecting to %s:%d: %w", host, port, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(n.timeout)
	_ = conn.SetDeadline(deadline)

	if len(data) > 0 {
		if _, err := conn.Write(data); err != nil {
			return fmt.Errorf("writing to %s:%d: %w", host, port, err)
		}
	}

	if n.out == "immed" {
		out.Status(node.Status{})
		return nil
	}

	raw, err := n.readReply(conn)
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "no reply"})
		return fmt.Errorf("reading from %s:%d: %w", host, port, err)
	}

	payload, err := socketPayload(raw, n.datatype)
	if err != nil {
		return err
	}
	m.SetPayload(payload)
	out.Status(node.Status{})
	out.Send(0, m)
	return nil
}

func (n *tcpRequestNode) readReply(conn net.Conn) ([]byte, error) {
	switch n.out {
	case "count":
		count, _ := strconv.Atoi(strings.TrimSpace(n.splitc))
		buf := make([]byte, count)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return nil, err
		}
		return buf, nil

	case "char":
		delim := []byte(unescapeDelimiter(n.splitc))
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64<<10), maxSocketFrameBytes)
		sc.Split(splitOnDelimiter(delim))
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		frame := sc.Bytes()
		outBuf := make([]byte, len(frame))
		copy(outBuf, frame)
		return outBuf, nil

	default:
		// "sit" reads until the peer closes; "time" reads until the deadline,
		// which surfaces as a timeout error with whatever arrived so far.
		raw, err := io.ReadAll(io.LimitReader(conn, maxSocketFrameBytes))
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() && n.out == "time" {
				// Expected: the mode is "whatever arrived within a time".
				return raw, nil
			}
			return nil, err
		}
		return raw, nil
	}
}

// ---------------------------------------------------------------------------
// udp in
// ---------------------------------------------------------------------------

type udpInNode struct {
	port      int
	iface     string
	multicast string // "false", "group"
	group     string
	datatype  string

	mu   sync.Mutex
	conn *net.UDPConn
}

func registerUDPIn() {
	node.MustRegister(node.Descriptor{
		Type:         "udp in",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "bridge",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "udp in",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Receives datagrams, optionally joining a multicast group, with " +
				"buffer, string or base64 payloads and the sender's address on msg.ip " +
				"and msg.port. IPv6 and per-node interface selection are not " +
				"implemented in this build.",
			UnsupportedProps: []string{"ipv6"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Required: true},
			{Name: "multicast", Kind: node.PropSelect, Label: "Listen for", Default: "false",
				Options: []node.Option{
					{Value: "false", Label: "Datagrams sent to this host"},
					{Value: "group", Label: "A multicast group"},
				}},
			{Name: "group", Kind: node.PropString, Label: "Multicast group",
				Placeholder: "239.255.0.1"},
			{Name: "iface", Kind: node.PropString, Label: "Interface address",
				Help: "Which interface to join the group on. Matters in macvlan mode, " +
					"where the pod has more than one."},
			{Name: "datatype", Kind: node.PropSelect, Label: "Payload", Default: "buffer",
				Options: []node.Option{
					{Value: "buffer", Label: "Raw bytes"},
					{Value: "utf8", Label: "A string"},
					{Value: "base64", Label: "base64"},
				}},
		},
		Help: "Receives UDP datagrams. Joining a multicast group is how a flow picks " +
			"up device announcements on an OT segment.",
	}, newUDPIn)
}

func newUDPIn(def *node.Definition) (node.Node, error) {
	n := &udpInNode{
		port:      def.Node.PropInt("port", 0),
		iface:     strings.TrimSpace(def.Node.PropString("iface", "")),
		multicast: def.Node.PropString("multicast", "false"),
		group:     strings.TrimSpace(def.Node.PropString("group", "")),
		datatype:  orDefault(def.Node.PropString("datatype", ""), "buffer"),
	}
	if err := validatePort(n.port); err != nil {
		return nil, err
	}
	if _, err := socketPayload(nil, n.datatype); err != nil {
		return nil, err
	}
	if n.multicast == "group" {
		if n.group == "" {
			return nil, fmt.Errorf("no multicast group configured")
		}
		ip := net.ParseIP(n.group)
		if ip == nil || !ip.IsMulticast() {
			return nil, fmt.Errorf("%q is not a multicast address", n.group)
		}
	}
	return n, nil
}

func (n *udpInNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *udpInNode) Start(ctx context.Context, out node.Emitter) error {
	var (
		conn *net.UDPConn
		err  error
	)

	if n.multicast == "group" {
		addr := &net.UDPAddr{IP: net.ParseIP(n.group), Port: n.port}
		var iface *net.Interface
		if n.iface != "" {
			iface, err = interfaceForAddress(n.iface)
			if err != nil {
				return err
			}
		}
		conn, err = net.ListenMulticastUDP("udp4", iface, addr)
	} else {
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: n.port})
	}
	if err != nil {
		return fmt.Errorf("listening on UDP port %d: %w", n.port, err)
	}

	n.mu.Lock()
	n.conn = conn
	n.mu.Unlock()

	out.Status(node.Status{Fill: "green", Shape: "ring", Text: "listening on " + conn.LocalAddr().String()})

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	go func() {
		buf := make([]byte, maxDatagramBytes)
		for {
			read, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				if ctx.Err() == nil {
					out.Error(fmt.Errorf("reading UDP: %w", err), nil)
				}
				return
			}
			// Copied out of the shared read buffer: the message outlives this
			// iteration and the next read overwrites it.
			raw := make([]byte, read)
			copy(raw, buf[:read])

			payload, err := socketPayload(raw, n.datatype)
			if err != nil {
				out.Error(err, nil)
				continue
			}
			m := engine.NewMsgWithPayload(payload)
			m.Data["ip"] = from.IP.String()
			m.Data["port"] = float64(from.Port)
			out.Send(0, m)
		}
	}()
	return nil
}

func (n *udpInNode) Close(context.Context, bool) error {
	n.mu.Lock()
	conn := n.conn
	n.conn = nil
	n.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return nil
}

// interfaceForAddress finds the interface holding an address, which is how a
// multicast join is pointed at the OT VLAN rather than at whichever interface
// the kernel picks first.
func interfaceForAddress(addr string) (*net.Interface, error) {
	want := net.ParseIP(addr)
	if want == nil {
		return nil, fmt.Errorf("%q is not an IP address", addr)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if ok && ipnet.IP.Equal(want) {
				return &ifaces[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no interface has the address %s", addr)
}

// ---------------------------------------------------------------------------
// udp out
// ---------------------------------------------------------------------------

type udpOutNode struct {
	addr      string
	port      int
	multicast string // "false", "broad", "multi"
	iface     string
	base64    bool

	mu   sync.Mutex
	conn *net.UDPConn
}

func registerUDPOut() {
	node.MustRegister(node.Descriptor{
		Type:         "udp out",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "bridge",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "udp out",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Sends datagrams to a host, a broadcast address or a multicast group, " +
				"with msg.ip and msg.port overriding the node. IPv6 is not implemented " +
				"in this build.",
			UnsupportedProps: []string{"ipv6"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "addr", Kind: node.PropString, Label: "Address",
				Help: "Leave empty to take it from msg.ip."},
			{Name: "port", Kind: node.PropNumber, Label: "Port"},
			{Name: "multicast", Kind: node.PropSelect, Label: "Send to", Default: "false",
				Options: []node.Option{
					{Value: "false", Label: "One host"},
					{Value: "broad", Label: "A broadcast address"},
					{Value: "multi", Label: "A multicast group"},
				}},
			{Name: "iface", Kind: node.PropString, Label: "Interface address"},
			{Name: "base64", Kind: node.PropBool, Label: "Decode the payload from base64"},
		},
		Help: "Sends the payload as a UDP datagram.",
	}, newUDPOut)
}

func newUDPOut(def *node.Definition) (node.Node, error) {
	n := &udpOutNode{
		addr:      strings.TrimSpace(def.Node.PropString("addr", "")),
		port:      def.Node.PropInt("port", 0),
		multicast: def.Node.PropString("multicast", "false"),
		iface:     strings.TrimSpace(def.Node.PropString("iface", "")),
		base64:    def.Node.PropBool("base64", false),
	}
	switch n.multicast {
	case "false", "broad", "multi":
	default:
		return nil, fmt.Errorf("unknown send mode %q", n.multicast)
	}
	if n.port != 0 {
		if err := validatePort(n.port); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *udpOutNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	addr := n.addr
	if v, ok := m.Data["ip"].(string); ok && v != "" {
		addr = v
	}
	port := n.port
	if f, ok := asFloat(m.Data["port"]); ok && f > 0 {
		port = int(f)
	}
	if addr == "" {
		return fmt.Errorf("no address configured and msg.ip is not set")
	}
	if err := validatePort(port); err != nil {
		return err
	}

	var (
		data []byte
		err  error
	)
	if n.base64 {
		s, ok := m.Payload().(string)
		if !ok {
			return fmt.Errorf("the node is set to decode base64 but the payload is %T", m.Payload())
		}
		if data, err = base64.StdEncoding.DecodeString(s); err != nil {
			return fmt.Errorf("the payload is not valid base64: %w", err)
		}
	} else if data, err = socketBytes(m.Payload()); err != nil {
		return err
	}

	if len(data) > maxDatagramBytes {
		return fmt.Errorf("the payload is %d bytes; a UDP datagram holds at most %d",
			len(data), maxDatagramBytes)
	}

	conn, err := n.socket()
	if err != nil {
		return err
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		resolved, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(addr, strconv.Itoa(port)))
		if err != nil {
			return fmt.Errorf("resolving %s: %w", addr, err)
		}
		ip = resolved.IP
	}
	if n.multicast == "multi" && !ip.IsMulticast() {
		return fmt.Errorf("%s is not a multicast address but the node is set to multicast", addr)
	}

	if _, err := conn.WriteToUDP(data, &net.UDPAddr{IP: ip, Port: port}); err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "send failed"})
		return fmt.Errorf("sending to %s:%d: %w", addr, port, err)
	}
	out.Status(node.Status{})
	return nil
}

// socket returns the sending socket, opening it once and keeping it. Opening one
// per message would burn an ephemeral port per datagram, which on a busy flow
// exhausts the range in minutes.
func (n *udpOutNode) socket() (*net.UDPConn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn != nil {
		return n.conn, nil
	}

	local := &net.UDPAddr{}
	if n.iface != "" {
		ip := net.ParseIP(n.iface)
		if ip == nil {
			return nil, fmt.Errorf("%q is not an IP address", n.iface)
		}
		local.IP = ip
	}
	conn, err := net.ListenUDP("udp4", local)
	if err != nil {
		return nil, fmt.Errorf("opening a UDP socket: %w", err)
	}
	n.conn = conn
	return conn, nil
}

func (n *udpOutNode) Close(context.Context, bool) error {
	n.mu.Lock()
	conn := n.conn
	n.conn = nil
	n.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return nil
}

// validatePort refuses a port a flow could not have meant.
func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%d is not a valid port", port)
	}
	return nil
}
