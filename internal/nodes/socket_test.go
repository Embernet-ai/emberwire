package nodes

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
)

// freePort asks the kernel for a port nobody is using, so the tests do not
// collide with each other or with whatever else is on the box.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("finding a free UDP port: %v", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// ---------------------------------------------------------------------------
// tcp in
// ---------------------------------------------------------------------------

func TestTCPInStreamMode(t *testing.T) {
	port := freePort(t)
	cfg, err := jsonConfig(map[string]any{
		"server": "server", "port": port, "datamode": "stream",
		"datatype": "utf8", "newline": `\n`, "topic": "line3",
	})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "tcp in", cfg, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	conn := dialWithRetry(t, port)
	defer conn.Close()

	if _, err := conn.Write([]byte("first\nsecond\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, 10*time.Second, "both delimited messages", func() bool { return e.total() == 2 })

	sent := e.on(0)
	if sent[0].Payload() != "first" || sent[1].Payload() != "second" {
		t.Fatalf("payloads %v, %v", sent[0].Payload(), sent[1].Payload())
	}
	if sent[0].Topic() != "line3" {
		t.Errorf("topic = %q", sent[0].Topic())
	}
	if sent[0].Data["ip"] == nil || sent[0].Data["port"] == nil {
		t.Errorf("the peer address is missing: %#v", sent[0].Data)
	}
	// A reply needs to find the connection it came from.
	sess, ok := sent[0].Data["_session"].(map[string]any)
	if !ok || sess["id"] == "" {
		t.Fatalf("msg._session = %#v", sent[0].Data["_session"])
	}
}

func TestTCPInSingleMode(t *testing.T) {
	port := freePort(t)
	cfg, err := jsonConfig(map[string]any{
		"server": "server", "port": port, "datamode": "single", "datatype": "utf8",
	})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "tcp in", cfg, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	conn := dialWithRetry(t, port)
	if _, err := conn.Write([]byte("no delimiter here")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Single mode delivers when the peer closes, not before.
	if e.total() != 0 {
		t.Fatal("single mode emitted before the connection closed")
	}
	conn.Close()

	waitFor(t, 10*time.Second, "the whole connection as one message",
		func() bool { return e.total() == 1 })
	if got := e.on(0)[0].Payload(); got != "no delimiter here" {
		t.Fatalf("payload = %q", got)
	}
}

// A raw payload is ImmutableBytes, so fanning a frame across several wires
// shares the buffer instead of copying it per wire.
func TestTCPInRawPayloadIsShareable(t *testing.T) {
	port := freePort(t)
	cfg, err := jsonConfig(map[string]any{
		"server": "server", "port": port, "datamode": "stream", "newline": `\n`,
	})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "tcp in", cfg, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	conn := dialWithRetry(t, port)
	defer conn.Close()
	if _, err := conn.Write([]byte("\x01\x02\x03\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, 10*time.Second, "the frame", func() bool { return e.total() == 1 })

	buf, ok := e.on(0)[0].Payload().(engine.ImmutableBytes)
	if !ok {
		t.Fatalf("payload is %T, want engine.ImmutableBytes", e.on(0)[0].Payload())
	}
	if len(buf) != 3 || buf[0] != 1 {
		t.Fatalf("payload = %#v", buf)
	}
}

// The reply has to go back down the connection the request arrived on, and the
// two nodes may be on different tabs with nothing in the flow graph connecting
// them.
func TestTCPOutRepliesOnTheSameConnection(t *testing.T) {
	port := freePort(t)
	inCfg, err := jsonConfig(map[string]any{
		"server": "server", "port": port, "datamode": "stream",
		"datatype": "utf8", "newline": `\n`,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := build(t, "tcp in", inCfg, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	defer cancel()
	t.Cleanup(TCPReplies.Reset)

	out := build(t, "tcp out", `{"beserver":"reply"}`, newTestServices())

	conn := dialWithRetry(t, port)
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, 10*time.Second, "the request", func() bool { return e.total() == 1 })

	m := e.on(0)[0]
	m.SetPayload("pong\n")
	if _, err := send(t, out, m); err != nil {
		t.Fatalf("reply: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 16)
	read, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading the reply: %v", err)
	}
	if string(buf[:read]) != "pong\n" {
		t.Fatalf("reply = %q", buf[:read])
	}
}

func TestTCPOutRefusesAReplyWithNoSession(t *testing.T) {
	out := build(t, "tcp out", `{"beserver":"reply"}`, newTestServices())
	if _, err := send(t, out, msg(t, `{"payload":"x"}`)); err == nil {
		t.Fatal("a reply with no msg._session was accepted")
	}
}

func TestTCPOutClientSends(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 32)
		read, err := conn.Read(buf)
		if err != nil {
			return
		}
		got <- string(buf[:read])
	}()

	cfg, err := jsonConfig(map[string]any{"beserver": "client", "host": "127.0.0.1", "port": port})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "tcp out", cfg, newTestServices())

	if _, err := send(t, n, msg(t, `{"payload":"hello"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case s := <-got:
		if s != "hello" {
			t.Fatalf("the server saw %q", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived at the server")
	}
}

// A Function node building a Modbus or EtherNet/IP frame produces an array of
// byte values, and it has to go on the wire as bytes rather than as its JSON.
func TestSocketBytesAcceptsAByteArray(t *testing.T) {
	got, err := socketBytes([]any{float64(0), float64(1), float64(255)})
	if err != nil {
		t.Fatalf("socketBytes: %v", err)
	}
	if len(got) != 3 || got[2] != 255 {
		t.Fatalf("bytes = %#v", got)
	}
	if _, err := socketBytes([]any{float64(300)}); err == nil {
		t.Fatal("300 was accepted as a byte")
	}
}

// ---------------------------------------------------------------------------
// tcp request
// ---------------------------------------------------------------------------

func TestTCPRequestWaitModes(t *testing.T) {
	// An echo server that answers with a fixed, delimited reply.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 64)
				if _, err := conn.Read(buf); err != nil {
					return
				}
				_, _ = conn.Write([]byte("ACK;trailing"))
			}()
		}
	}()

	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{
			name: "up to a delimiter",
			cfg:  map[string]any{"out": "char", "splitc": ";"},
			want: "ACK",
		},
		{
			name: "a fixed byte count",
			cfg:  map[string]any{"out": "count", "splitc": "3"},
			want: "ACK",
		},
		{
			name: "until the peer closes",
			cfg:  map[string]any{"out": "sit"},
			want: "ACK;trailing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := map[string]any{
				"host": "127.0.0.1", "port": port, "datatype": "utf8", "ew_timeout": 10,
			}
			for k, v := range tc.cfg {
				cfg[k] = v
			}
			s, err := jsonConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			n := build(t, "tcp request", s, newTestServices())

			e, err := send(t, n, msg(t, `{"payload":"REQ"}`))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if got := e.on(0)[0].Payload(); got != tc.want {
				t.Fatalf("payload = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTCPRequestRefusesBadConfiguration(t *testing.T) {
	cases := map[string]map[string]any{
		"unknown wait mode":    {"out": "eventually"},
		"count with no number": {"out": "count", "splitc": "nope"},
		"char with no char":    {"out": "char", "splitc": ""},
		"unknown data type":    {"datatype": "yaml"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := map[string]any{"host": "127.0.0.1", "port": 9999}
			for k, v := range extra {
				cfg[k] = v
			}
			s, err := jsonConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := buildErr(t, "tcp request", s, newTestServices()); err == nil {
				t.Fatal("expected the node to refuse to build")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// udp
// ---------------------------------------------------------------------------

func TestUDPRoundTrip(t *testing.T) {
	port := freeUDPPort(t)

	inCfg, err := jsonConfig(map[string]any{"port": port, "datatype": "utf8"})
	if err != nil {
		t.Fatal(err)
	}
	in := build(t, "udp in", inCfg, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	defer cancel()

	outCfg, err := jsonConfig(map[string]any{"addr": "127.0.0.1", "port": port})
	if err != nil {
		t.Fatal(err)
	}
	out := build(t, "udp out", outCfg, newTestServices())

	// UDP is lossy and the listener may not be bound yet, so send until one
	// lands rather than asserting on a single datagram.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && e.total() == 0 {
		if _, err := send(t, out, msg(t, `{"payload":"ping"}`)); err != nil {
			t.Fatalf("send: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e.total() == 0 {
		t.Fatal("no datagram arrived")
	}

	m := e.on(0)[0]
	if m.Payload() != "ping" {
		t.Fatalf("payload = %v", m.Payload())
	}
	if m.Data["ip"] != "127.0.0.1" {
		t.Errorf("msg.ip = %v", m.Data["ip"])
	}
	if _, ok := m.Data["port"].(float64); !ok {
		t.Errorf("msg.port = %#v", m.Data["port"])
	}
}

func TestUDPOutTakesTheAddressFromTheMessage(t *testing.T) {
	port := freeUDPPort(t)
	inCfg, err := jsonConfig(map[string]any{"port": port, "datatype": "utf8"})
	if err != nil {
		t.Fatal(err)
	}
	in := build(t, "udp in", inCfg, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	defer cancel()

	// No address on the node at all: it comes from msg.ip and msg.port, which is
	// how one node talks to a device list.
	out := build(t, "udp out", `{}`, newTestServices())
	m, err := jsonConfig(map[string]any{"payload": "from-msg", "ip": "127.0.0.1", "port": port})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && e.total() == 0 {
		if _, err := send(t, out, msg(t, m)); err != nil {
			t.Fatalf("send: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e.total() == 0 {
		t.Fatal("no datagram arrived")
	}
	if got := e.on(0)[0].Payload(); got != "from-msg" {
		t.Fatalf("payload = %v", got)
	}
}

func TestUDPRefusesBadConfiguration(t *testing.T) {
	t.Run("a multicast group that is not multicast", func(t *testing.T) {
		cfg, err := jsonConfig(map[string]any{
			"port": 12345, "multicast": "group", "group": "10.0.0.1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := buildErr(t, "udp in", cfg, newTestServices()); err == nil {
			t.Fatal("a unicast address was accepted as a multicast group")
		}
	})

	t.Run("a port out of range", func(t *testing.T) {
		cfg, err := jsonConfig(map[string]any{"port": 70000})
		if err != nil {
			t.Fatal(err)
		}
		if err := buildErr(t, "udp in", cfg, newTestServices()); err == nil {
			t.Fatal("port 70000 was accepted")
		}
	})

	t.Run("no address anywhere", func(t *testing.T) {
		out := build(t, "udp out", `{"port":9999}`, newTestServices())
		if _, err := send(t, out, msg(t, `{"payload":"x"}`)); err == nil {
			t.Fatal("a send with no address was accepted")
		}
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// dialWithRetry connects, retrying while the node's listener comes up.
func dialWithRetry(t *testing.T, port int) net.Conn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			return conn
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("could not connect to port %d", port)
	return nil
}

func TestUnescapeDelimiter(t *testing.T) {
	// Node-RED's edit dialog stores a newline as the two characters \ and n, so
	// a delimiter that is not unescaped never matches and every stream reads as
	// one enormous message.
	for in, want := range map[string]string{
		`\n`:   "\n",
		`\r\n`: "\r\n",
		`\t`:   "\t",
		`;`:    ";",
		`\0`:   "\x00",
	} {
		if got := unescapeDelimiter(in); got != want {
			t.Errorf("unescapeDelimiter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitOnDelimiterHandlesMultiByte(t *testing.T) {
	// A plant-floor protocol is as likely to terminate on a two-byte sequence as
	// on a newline, and bufio.ScanLines only knows about the latter.
	split := splitOnDelimiter([]byte("\r\n"))
	adv, tok, err := split([]byte("abc\r\ndef"), false)
	if err != nil || adv != 5 || string(tok) != "abc" {
		t.Fatalf("split = %d, %q, %v", adv, tok, err)
	}
	// Nothing yet: wait for more rather than emitting a partial frame.
	adv, tok, err = split([]byte("def"), false)
	if err != nil || adv != 0 || tok != nil {
		t.Fatalf("incomplete split = %d, %q, %v", adv, tok, err)
	}
	// At EOF whatever arrived is still data.
	adv, tok, err = split([]byte("def"), true)
	if err != nil || adv != 3 || string(tok) != "def" {
		t.Fatalf("EOF split = %d, %q, %v", adv, tok, err)
	}
}
