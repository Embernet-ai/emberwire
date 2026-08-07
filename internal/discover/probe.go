package discover

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// Well-known OT ports. These are what a discovery sweep is actually looking for
// on a plant floor; a generic 1-65535 scan is both slower and less useful.
const (
	PortModbus     = 502
	PortOPCUA      = 4840
	PortEtherNetIP = 44818
	PortBACnet     = 47808
	PortS7         = 102
	PortFINS       = 9600
	PortHTTP       = 80
	PortHTTPS      = 443
	PortSSH        = 22
	PortMQTT       = 1883
)

// DefaultPorts is the sweep used when a node does not narrow it.
var DefaultPorts = []int{
	PortSSH, PortHTTP, PortS7, PortHTTPS, PortModbus,
	PortMQTT, PortOPCUA, PortFINS, PortEtherNetIP, PortBACnet,
}

// PortName labels a port in the output, so an operator reading a device list
// sees "modbus" rather than 502.
func PortName(p int) string {
	switch p {
	case PortModbus:
		return "modbus"
	case PortOPCUA:
		return "opc-ua"
	case PortEtherNetIP:
		return "ethernet-ip"
	case PortBACnet:
		return "bacnet"
	case PortS7:
		return "s7"
	case PortFINS:
		return "omron-fins"
	case PortHTTP:
		return "http"
	case PortHTTPS:
		return "https"
	case PortSSH:
		return "ssh"
	case PortMQTT:
		return "mqtt"
	default:
		return "tcp/" + strconv.Itoa(p)
	}
}

// Options tune a sweep.
type Options struct {
	// Timeout bounds one probe.
	Timeout time.Duration
	// Concurrency bounds simultaneous probes. OT links are frequently 100Mb and
	// shared with the process traffic that matters, so this defaults low: a
	// discovery scan must never be the reason a line stutters.
	Concurrency int
	// Ports to probe on each host.
	Ports []int
	// ResolveNames does a reverse DNS lookup per responding host.
	ResolveNames bool
	// Identify runs the protocol-specific identification probes against ports
	// that answered.
	Identify bool
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 700 * time.Millisecond
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 32
	}
	if len(o.Ports) == 0 {
		o.Ports = DefaultPorts
	}
	return o
}

// Sweep probes every address in a range and returns what answered.
//
// TCP connect rather than raw SYN. A connect scan needs no elevated capability,
// works identically inside a container with capabilities dropped, and — more
// importantly on a plant floor — completes the handshake rather than leaving
// half-open connections on a PLC whose TCP stack may have a connection table of
// eight.
func Sweep(ctx context.Context, scope *Scope, prefix netip.Prefix, opts Options) ([]Device, error) {
	if err := scope.CheckPrefix(prefix); err != nil {
		return nil, err
	}
	opts = opts.withDefaults()

	hosts, err := Hosts(prefix)
	if err != nil {
		return nil, err
	}

	collector := NewCollector()
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup

	for _, addr := range hosts {
		for _, port := range opts.Ports {
			select {
			case <-ctx.Done():
				// Return what was found rather than nothing. A sweep cancelled
				// half way through still tells the operator something.
				wg.Wait()
				return collector.Devices(), ctx.Err()
			default:
			}

			wg.Add(1)
			go func(a netip.Addr, p int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				probeOne(ctx, a, p, opts, collector)
			}(addr, port)
		}
	}
	wg.Wait()

	if opts.ResolveNames {
		resolveNames(ctx, collector, opts)
	}
	return collector.Devices(), nil
}

func probeOne(ctx context.Context, addr netip.Addr, port int, opts Options, c *Collector) {
	target := net.JoinHostPort(addr.String(), strconv.Itoa(port))

	dialCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", target)
	if err != nil {
		return
	}

	dev := Device{
		Address: addr.String(),
		Ports:   []int{port},
		Source:  "port-scan",
		Details: map[string]any{PortName(port): true},
	}

	if opts.Identify {
		if info := identify(dialCtx, conn, port); info != nil {
			dev.Protocol = info.Protocol
			if dev.Details == nil {
				dev.Details = map[string]any{}
			}
			for k, v := range info.Details {
				dev.Details[k] = v
			}
		}
	}
	_ = conn.Close()

	c.Add(dev)
}

func resolveNames(ctx context.Context, c *Collector, opts Options) {
	devices := c.Devices()
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup

	for _, d := range devices {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			lookupCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
			defer cancel()

			var r net.Resolver
			names, err := r.LookupAddr(lookupCtx, addr)
			if err != nil || len(names) == 0 {
				return
			}
			c.Add(Device{Address: addr, Hostname: trimDot(names[0]), Source: "rdns"})
		}(d.Address)
	}
	wg.Wait()
}

func trimDot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}

// Identification is what a protocol probe learned about a device.
type Identification struct {
	Protocol string
	Details  map[string]any
}

// identify runs a protocol-specific handshake on an open connection.
//
// Only for ports where a short, safe, read-only request exists. Nothing here
// writes to a device: a discovery scan that can change a coil is not a
// discovery scan.
func identify(ctx context.Context, conn net.Conn, port int) *Identification {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Second)
	}
	_ = conn.SetDeadline(deadline)

	switch port {
	case PortModbus:
		return identifyModbus(conn)
	case PortEtherNetIP:
		return identifyEtherNetIP(conn)
	default:
		return nil
	}
}

// identifyModbus sends a Read Device Identification request.
//
// Function code 43 / MEI type 14, "Read Device Identification", basic category.
// It is defined by the Modbus specification as read-only and is the only
// identification request that does not touch process data. A device that does
// not implement it answers with an exception, which still confirms it speaks
// Modbus — so an exception response is a positive result here, not a failure.
func identifyModbus(conn net.Conn) *Identification {
	req := []byte{
		0x00, 0x01, // transaction id
		0x00, 0x00, // protocol id (0 = Modbus)
		0x00, 0x05, // length of the remaining bytes
		0x01, // unit id
		0x2B, // function 43
		0x0E, // MEI type 14
		0x01, // read device id: basic
		0x00, // object id
	}
	if _, err := conn.Write(req); err != nil {
		return nil
	}

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil || n < 8 {
		return nil
	}
	// Protocol identifier must be zero for Modbus TCP. Anything else on 502 is
	// something other than a PLC.
	if binary.BigEndian.Uint16(buf[2:4]) != 0 {
		return nil
	}

	details := map[string]any{"unitId": float64(buf[6])}
	fn := buf[7]
	if fn == 0xAB {
		// Exception response: still Modbus, just no device identification.
		details["deviceIdSupported"] = false
		if n >= 9 {
			details["exceptionCode"] = float64(buf[8])
		}
		return &Identification{Protocol: "modbus", Details: details}
	}
	if fn != 0x2B {
		return nil
	}
	details["deviceIdSupported"] = true
	for k, v := range parseModbusDeviceID(buf[:n]) {
		details[k] = v
	}
	return &Identification{Protocol: "modbus", Details: details}
}

// parseModbusDeviceID walks the object list of a Read Device Identification
// response. Objects 0, 1 and 2 are vendor, product code and revision.
func parseModbusDeviceID(resp []byte) map[string]any {
	out := map[string]any{}
	// Byte layout, counting from zero:
	//   0-6   MBAP header (transaction, protocol, length, unit id)
	//   7     function code (0x2B)
	//   8     MEI type (0x0E)
	//   9     read device id code
	//   10    conformity level
	//   11    more follows
	//   12    next object id
	//   13    number of objects
	//   14+   the objects themselves
	const objectsAt = 14
	if len(resp) < objectsAt {
		return out
	}
	count := int(resp[objectsAt-1])
	names := map[byte]string{0: "vendor", 1: "productCode", 2: "revision"}

	pos := objectsAt
	for i := 0; i < count && pos+2 <= len(resp); i++ {
		id := resp[pos]
		length := int(resp[pos+1])
		pos += 2
		if pos+length > len(resp) {
			break
		}
		value := string(resp[pos : pos+length])
		pos += length
		if name, ok := names[id]; ok {
			out[name] = value
		}
	}
	return out
}

// identifyEtherNetIP sends a ListIdentity request.
//
// Command 0x0063 over the encapsulation layer. Read-only by definition — it is
// what every EtherNet/IP browser sends — and the response carries the vendor id,
// device type, product name and serial number.
func identifyEtherNetIP(conn net.Conn) *Identification {
	req := make([]byte, 24)
	binary.LittleEndian.PutUint16(req[0:2], 0x0063) // ListIdentity
	// Length 0, session 0, status 0, sender context 0, options 0.

	if _, err := conn.Write(req); err != nil {
		return nil
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil || n < 24 {
		return nil
	}
	if binary.LittleEndian.Uint16(buf[0:2]) != 0x0063 {
		return nil
	}

	details := map[string]any{}
	// Encapsulation header 24 + item count 2 + item type 2 + item length 2 +
	// protocol version 2 + socket address 16 = 48 before the identity fields.
	const identityAt = 48
	if n >= identityAt+14 {
		details["vendorId"] = float64(binary.LittleEndian.Uint16(buf[identityAt : identityAt+2]))
		details["deviceType"] = float64(binary.LittleEndian.Uint16(buf[identityAt+2 : identityAt+4]))
		details["productCode"] = float64(binary.LittleEndian.Uint16(buf[identityAt+4 : identityAt+6]))
		details["revision"] = fmt.Sprintf("%d.%d", buf[identityAt+6], buf[identityAt+7])
		details["serialNumber"] = fmt.Sprintf("%08X",
			binary.LittleEndian.Uint32(buf[identityAt+10:identityAt+14]))

		nameAt := identityAt + 14
		if nameAt < n {
			nameLen := int(buf[nameAt])
			if nameAt+1+nameLen <= n {
				details["productName"] = string(buf[nameAt+1 : nameAt+1+nameLen])
			}
		}
	}
	return &Identification{Protocol: "ethernet-ip", Details: details}
}

// Interfaces reports the host's network interfaces.
//
// In macvlan mode this is how a flow learns the address it was given on the OT
// VLAN, which it needs in order to know what to sweep.
func Interfaces() ([]map[string]any, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(ifaces))
	for _, iface := range ifaces {
		entry := map[string]any{
			"name":     iface.Name,
			"mtu":      float64(iface.MTU),
			"up":       iface.Flags&net.FlagUp != 0,
			"loopback": iface.Flags&net.FlagLoopback != 0,
		}
		if iface.HardwareAddr != nil {
			entry["mac"] = iface.HardwareAddr.String()
		}

		addrs, err := iface.Addrs()
		if err == nil {
			list := make([]any, 0, len(addrs))
			for _, a := range addrs {
				list = append(list, a.String())
			}
			entry["addresses"] = list
		}
		out = append(out, entry)
	}
	return out, nil
}
