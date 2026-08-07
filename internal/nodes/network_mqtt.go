package nodes

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

const colorNetwork = "#D8BFD8"

func init() {
	registerMQTTBroker()
	registerMQTTIn()
	registerMQTTOut()
}

// MQTTBroker is how in and out nodes reach their shared connection.
type MQTTBroker interface {
	Client() mqtt.Client
	Subscribe(topic string, qos byte, h mqtt.MessageHandler) error
	Unsubscribe(topic string) error
}

// mqttBrokerConfig owns one connection, shared by every node that references it.
type mqttBrokerConfig struct {
	opts     *mqtt.ClientOptions
	clientID string

	// closeTopic is published on a clean disconnect, so downstream systems see
	// the app go away deliberately rather than inferring it from a timeout.
	closeTopic   string
	closePayload string

	mu     sync.Mutex
	client mqtt.Client
	// subs is replayed on reconnect. Paho resubscribes automatically only when
	// clean session is off; keeping our own record makes the behaviour the same
	// either way, which matters because an edge link drops constantly.
	subs map[string]mqttSub
}

type mqttSub struct {
	qos     byte
	handler mqtt.MessageHandler
}

func registerMQTTBroker() {
	node.MustRegister(node.Descriptor{
		Type:     "mqtt-broker",
		Category: node.CategoryConfig,
		Color:    colorNetwork,
		Icon:     "mqtt",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Connection, credentials, TLS, clean session, keepalive, birth and " +
				"close messages are supported. Will messages and MQTT v5 properties " +
				"are not implemented in this build.",
			UnsupportedProps: []string{"willTopic", "willPayload", "protocolVersion:5"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "broker", Kind: node.PropString, Label: "Server", Required: true,
				Placeholder: "monster-mq.tenant-fireball.svc.cluster.local"},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Default: 1883},
			{Name: "tls", Kind: node.PropBool, Label: "Use TLS"},
			{Name: "verifyservercert", Kind: node.PropBool, Label: "Verify the server certificate",
				Default: true},
			{Name: "clientid", Kind: node.PropString, Label: "Client ID",
				Help: "Leave empty to generate one. Two clients sharing an ID disconnect each other."},
			{Name: "user", Kind: node.PropString, Label: "Username"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
			{Name: "cleansession", Kind: node.PropBool, Label: "Use a clean session", Default: true},
			{Name: "keepalive", Kind: node.PropNumber, Label: "Keep alive (seconds)", Default: 60},
			{Name: "birthTopic", Kind: node.PropString, Label: "Birth topic",
				Help: "Published on connect."},
			{Name: "birthPayload", Kind: node.PropString, Label: "Birth payload"},
			{Name: "closeTopic", Kind: node.PropString, Label: "Close topic",
				Help: "Published on a clean disconnect."},
			{Name: "closePayload", Kind: node.PropString, Label: "Close payload"},
		},
		Help: "Connection to an MQTT broker.",
	}, newMQTTBroker)
}

func newMQTTBroker(def *node.Definition) (node.Node, error) {
	host := def.Node.PropString("broker", "")
	if host == "" {
		return nil, fmt.Errorf("server is required")
	}
	port := def.Node.PropInt("port", 1883)
	useTLS := def.Node.PropBool("tls", false)

	scheme := "tcp"
	if useTLS {
		scheme = "ssl"
	}

	clientID := def.Node.PropString("clientid", "")
	if clientID == "" {
		// Derived from the node id rather than random, so a reconnect after a
		// restart resumes the same session instead of orphaning the old one.
		clientID = "emberwire-" + def.Node.ID
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("%s://%s:%d", scheme, host, port))
	opts.SetClientID(clientID)
	opts.SetCleanSession(def.Node.PropBool("cleansession", true))
	opts.SetKeepAlive(time.Duration(def.Node.PropInt("keepalive", 60)) * time.Second)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(30 * time.Second)
	opts.SetConnectTimeout(10 * time.Second)
	// Paho drops messages silently when its internal channel fills. Ordering
	// matters more than throughput for control traffic, so handlers run in
	// order on one goroutine and the scheduler's own bounded inbox provides
	// the back-pressure.
	opts.SetOrderMatters(true)

	if user := def.Node.PropString("user", ""); user != "" {
		opts.SetUsername(user)
		if pw, ok := def.Services.Credential("password"); ok {
			opts.SetPassword(pw)
		}
	}

	if useTLS {
		opts.SetTLSConfig(&tls.Config{
			// Defaults to verifying. Turning it off is a deliberate act because
			// an OT network with a self-signed broker is common, and quietly
			// defaulting to "trust anything" is how that becomes permanent.
			InsecureSkipVerify: !def.Node.PropBool("verifyservercert", true),
			MinVersion:         tls.VersionTLS12,
		})
	}

	c := &mqttBrokerConfig{opts: opts, clientID: clientID, subs: map[string]mqttSub{}}

	if bt := def.Node.PropString("birthTopic", ""); bt != "" {
		payload := def.Node.PropString("birthPayload", "")
		opts.SetOnConnectHandler(func(client mqtt.Client) {
			client.Publish(bt, 0, false, payload)
			c.replaySubscriptions(client)
		})
	} else {
		opts.SetOnConnectHandler(func(client mqtt.Client) {
			c.replaySubscriptions(client)
		})
	}

	if ct := def.Node.PropString("closeTopic", ""); ct != "" {
		c.closeTopic = ct
		c.closePayload = def.Node.PropString("closePayload", "")
	}

	return c, nil
}

func (c *mqttBrokerConfig) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

// Client returns the connected client, connecting on first use.
//
// Lazily, for the same reason the PostgreSQL pool is lazy: a broker that is not
// up yet must not stop the flow from starting. On an edge box the broker and
// the app usually boot together.
func (c *mqttBrokerConfig) Client() mqtt.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		c.client = mqtt.NewClient(c.opts)
	}
	if !c.client.IsConnected() {
		// Fire and forget: paho's auto-reconnect keeps trying, and blocking a
		// node's goroutine on a broker that is down would stall its inbox.
		c.client.Connect()
	}
	return c.client
}

// Subscribe registers a handler and applies it, remembering it for reconnects.
func (c *mqttBrokerConfig) Subscribe(topic string, qos byte, h mqtt.MessageHandler) error {
	c.mu.Lock()
	c.subs[topic] = mqttSub{qos: qos, handler: h}
	c.mu.Unlock()

	client := c.Client()
	if !client.IsConnected() {
		// Recorded and replayed by the connect handler once the link comes up.
		return nil
	}
	tok := client.Subscribe(topic, qos, h)
	if !tok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("timed out subscribing to %q", topic)
	}
	return tok.Error()
}

func (c *mqttBrokerConfig) Unsubscribe(topic string) error {
	c.mu.Lock()
	delete(c.subs, topic)
	client := c.client
	c.mu.Unlock()

	if client == nil || !client.IsConnected() {
		return nil
	}
	tok := client.Unsubscribe(topic)
	tok.WaitTimeout(5 * time.Second)
	return tok.Error()
}

func (c *mqttBrokerConfig) replaySubscriptions(client mqtt.Client) {
	c.mu.Lock()
	subs := make(map[string]mqttSub, len(c.subs))
	for t, s := range c.subs {
		subs[t] = s
	}
	c.mu.Unlock()

	for topic, s := range subs {
		client.Subscribe(topic, s.qos, s.handler)
	}
}

func (c *mqttBrokerConfig) Close(context.Context, bool) error {
	c.mu.Lock()
	client := c.client
	closeTopic, closePayload := c.closeTopic, c.closePayload
	c.client = nil
	c.mu.Unlock()

	if client == nil {
		return nil
	}
	if closeTopic != "" && client.IsConnected() {
		tok := client.Publish(closeTopic, 0, false, closePayload)
		tok.WaitTimeout(2 * time.Second)
	}
	// Give in-flight publishes a moment to leave before the socket closes.
	client.Disconnect(500)
	return nil
}

// ---------------------------------------------------------------------------
// mqtt in
// ---------------------------------------------------------------------------

type mqttInNode struct {
	cfgID   string
	topic   string
	qos     byte
	output  string // auto, buffer, string, json
	svc     node.Services
	broker  MQTTBroker
	started bool
}

func registerMQTTIn() {
	node.MustRegister(node.Descriptor{
		Type:         "mqtt in",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "mqtt",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "mqtt in",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Topic subscription with QoS and payload decoding are supported. " +
				"Dynamic subscription via a control message is not implemented.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "broker", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "mqtt-broker", Required: true},
			{Name: "topic", Kind: node.PropString, Label: "Topic", Required: true,
				Placeholder: "press/+/raw"},
			{Name: "qos", Kind: node.PropSelect, Label: "QoS", Default: "0", Options: []node.Option{
				{Value: "0", Label: "0 — at most once"},
				{Value: "1", Label: "1 — at least once"},
				{Value: "2", Label: "2 — exactly once"},
			}},
			{Name: "datatype", Kind: node.PropSelect, Label: "Output", Default: "auto",
				Options: []node.Option{
					{Value: "auto", Label: "Auto-detect — parse JSON, else a string"},
					{Value: "utf8", Label: "A string"},
					{Value: "json", Label: "A parsed JSON object"},
					{Value: "buffer", Label: "A binary buffer"},
				}},
		},
		Help: "Subscribes to an MQTT topic and emits a message for each one received.",
	}, newMQTTIn)
}

func newMQTTIn(def *node.Definition) (node.Node, error) {
	n := &mqttInNode{
		cfgID:  def.Node.PropString("broker", ""),
		topic:  def.Node.PropString("topic", ""),
		output: def.Node.PropString("datatype", "auto"),
		svc:    def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected")
	}
	if n.topic == "" {
		return nil, fmt.Errorf("topic is required")
	}
	q := def.Node.PropInt("qos", 0)
	if q < 0 || q > 2 {
		return nil, fmt.Errorf("qos must be 0, 1 or 2, got %d", q)
	}
	n.qos = byte(q)
	return n, nil
}

func (n *mqttInNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *mqttInNode) Start(ctx context.Context, out node.Emitter) error {
	cfg, ok := n.svc.ConfigNode(n.cfgID)
	if !ok {
		return fmt.Errorf("server config node %s is not running", n.cfgID)
	}
	b, ok := cfg.(MQTTBroker)
	if !ok {
		return fmt.Errorf("config node %s is not an MQTT broker", n.cfgID)
	}
	n.broker = b

	out.Status(node.Status{Fill: "yellow", Shape: "ring", Text: "connecting"})

	handler := func(_ mqtt.Client, msg mqtt.Message) {
		m := engine.NewMsg()
		m.SetTopic(msg.Topic())
		m.SetPayload(decodeMQTTPayload(msg.Payload(), n.output))
		m.Data["qos"] = float64(msg.Qos())
		m.Data["retain"] = msg.Retained()

		out.Status(node.Status{Fill: "green", Shape: "dot", Text: "connected"})
		out.Send(0, m)
	}

	if err := n.broker.Subscribe(n.topic, n.qos, handler); err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "subscribe failed"})
		return err
	}
	n.started = true
	return nil
}

func (n *mqttInNode) Close(context.Context, bool) error {
	if n.broker != nil && n.started {
		return n.broker.Unsubscribe(n.topic)
	}
	return nil
}

// decodeMQTTPayload turns bytes into what the flow asked for.
//
// "auto" tries JSON and falls back to a string, which is what makes a flow
// reading a JSON-publishing sensor work without configuration. A payload that
// is not valid UTF-8 stays a buffer, because silently producing a string full
// of replacement characters destroys binary data.
func decodeMQTTPayload(raw []byte, mode string) any {
	switch mode {
	case "buffer":
		return raw
	case "utf8":
		return string(raw)
	case "json":
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			// Returning the raw string rather than an error keeps one malformed
			// publish from stopping the subscription.
			return string(raw)
		}
		return v
	default: // auto
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var v any
			if err := json.Unmarshal(raw, &v); err == nil {
				return v
			}
		}
		if !isValidUTF8(raw) {
			return raw
		}
		return string(raw)
	}
}

func isValidUTF8(b []byte) bool {
	return strings.ToValidUTF8(string(b), "\uFFFD") == string(b)
}

// ---------------------------------------------------------------------------
// mqtt out
// ---------------------------------------------------------------------------

type mqttOutNode struct {
	cfgID  string
	topic  string
	qos    byte
	retain bool
	svc    node.Services
	broker MQTTBroker
}

func registerMQTTOut() {
	node.MustRegister(node.Descriptor{
		Type:         "mqtt out",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "mqtt",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "mqtt out",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Publishing with topic, QoS and retain from the node or the message " +
				"is supported. MQTT v5 user properties are not implemented.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "broker", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "mqtt-broker", Required: true},
			{Name: "topic", Kind: node.PropString, Label: "Topic",
				Help: "Leave empty to use msg.topic."},
			{Name: "qos", Kind: node.PropSelect, Label: "QoS", Default: "0", Options: []node.Option{
				{Value: "0", Label: "0 — at most once"},
				{Value: "1", Label: "1 — at least once"},
				{Value: "2", Label: "2 — exactly once"},
			}},
			{Name: "retain", Kind: node.PropBool, Label: "Retain"},
		},
		Help: "Publishes msg.payload to an MQTT topic.",
	}, newMQTTOut)
}

func newMQTTOut(def *node.Definition) (node.Node, error) {
	n := &mqttOutNode{
		cfgID:  def.Node.PropString("broker", ""),
		topic:  def.Node.PropString("topic", ""),
		retain: def.Node.PropBool("retain", false),
		svc:    def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected")
	}
	q := def.Node.PropInt("qos", 0)
	if q < 0 || q > 2 {
		return nil, fmt.Errorf("qos must be 0, 1 or 2, got %d", q)
	}
	n.qos = byte(q)
	return n, nil
}

func (n *mqttOutNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	if n.broker == nil {
		cfg, ok := n.svc.ConfigNode(n.cfgID)
		if !ok {
			return fmt.Errorf("server config node %s is not running", n.cfgID)
		}
		b, ok := cfg.(MQTTBroker)
		if !ok {
			return fmt.Errorf("config node %s is not an MQTT broker", n.cfgID)
		}
		n.broker = b
	}

	topic := n.topic
	if topic == "" {
		topic = m.Topic()
	}
	if topic == "" {
		return fmt.Errorf("no topic: set one on the node or on msg.topic")
	}

	qos := n.qos
	if v, ok, _ := m.Get("qos"); ok {
		if f, isNum := asFloat(v); isNum && f >= 0 && f <= 2 {
			qos = byte(f)
		}
	}
	retain := n.retain
	if v, ok, _ := m.Get("retain"); ok {
		if b, isBool := v.(bool); isBool {
			retain = b
		}
	}

	payload, err := encodeMQTTPayload(m.Payload())
	if err != nil {
		return err
	}

	client := n.broker.Client()
	if !client.IsConnected() {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "not connected"})
		return fmt.Errorf("broker is not connected")
	}

	tok := client.Publish(topic, qos, retain, payload)
	// QoS 0 is fire-and-forget by definition; waiting for it would add latency
	// for a confirmation the protocol never sends.
	if qos > 0 {
		if !tok.WaitTimeout(10 * time.Second) {
			return fmt.Errorf("timed out publishing to %q", topic)
		}
		if err := tok.Error(); err != nil {
			return fmt.Errorf("publishing to %q: %w", topic, err)
		}
	}

	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "published"})
	return nil
}

// encodeMQTTPayload renders a payload for the wire. Objects and arrays go as
// JSON, which is what every consumer on an industrial bus expects.
func encodeMQTTPayload(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return t, nil
	case engine.ImmutableBytes:
		return t, nil
	case string:
		return []byte(t), nil
	case bool:
		if t {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	default:
		if f, ok := asFloat(v); ok {
			// -1 precision so 21 publishes as "21" rather than "21.000000".
			return []byte(strconv.FormatFloat(f, 'f', -1, 64)), nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encoding payload: %w", err)
		}
		return b, nil
	}
}
