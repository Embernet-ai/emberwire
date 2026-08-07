package discover

import (
	"errors"
	"net/netip"
	"testing"
)

func mustScope(t *testing.T, cidrs ...string) *Scope {
	t.Helper()
	s, err := NewScope(true, cidrs)
	if err != nil {
		t.Fatalf("NewScope(%v): %v", cidrs, err)
	}
	return s
}

func TestScopeDisabledRefusesEverything(t *testing.T) {
	// The default. A scan node in a runtime where discovery was never switched
	// on must error rather than quietly return nothing, because an empty result
	// looks exactly like a network with nothing on it.
	s, err := NewScope(false, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	if s.Enabled() {
		t.Error("a disabled scope reports enabled")
	}
	if err := s.CheckAddr(netip.MustParseAddr("10.20.30.5")); !errors.Is(err, ErrDisabled) {
		t.Errorf("CheckAddr = %v, want ErrDisabled", err)
	}
	if err := s.CheckPrefix(netip.MustParsePrefix("10.20.30.0/24")); !errors.Is(err, ErrDisabled) {
		t.Errorf("CheckPrefix = %v, want ErrDisabled", err)
	}
}

func TestScopeEnabledWithNoCIDRsIsAnError(t *testing.T) {
	// The permissive reading of an empty list — "no restrictions" — is how a
	// tool meant to inventory one factory segment ends up scanning a corporate
	// network. It is a configuration error instead.
	if _, err := NewScope(true, nil); err == nil {
		t.Fatal("an enabled scope with no CIDRs was accepted")
	}
	if _, err := NewScope(true, []string{}); err == nil {
		t.Fatal("an enabled scope with an empty CIDR list was accepted")
	}
}

func TestScopeRejectsBadCIDR(t *testing.T) {
	for _, bad := range []string{"not-a-cidr", "10.20.30.0", "10.20.30.0/33", ""} {
		if _, err := NewScope(true, []string{bad}); err == nil {
			t.Errorf("NewScope accepted %q", bad)
		}
	}
}

func TestScopeMasksHostBitsInAllowedCIDR(t *testing.T) {
	// An operator writing 10.20.30.5/24 means that network. Without masking,
	// the prefix never matches anything and discovery silently does nothing.
	s := mustScope(t, "10.20.30.5/24")
	if err := s.CheckAddr(netip.MustParseAddr("10.20.30.99")); err != nil {
		t.Errorf("CheckAddr = %v, want nil — host bits were not masked", err)
	}
}

func TestScopeCheckAddr(t *testing.T) {
	s := mustScope(t, "10.20.30.0/24", "192.168.1.0/28")

	inScope := []string{"10.20.30.1", "10.20.30.254", "192.168.1.0", "192.168.1.15"}
	for _, a := range inScope {
		if err := s.CheckAddr(netip.MustParseAddr(a)); err != nil {
			t.Errorf("CheckAddr(%s) = %v, want nil", a, err)
		}
	}

	outOfScope := []string{"10.20.31.1", "192.168.1.16", "8.8.8.8", "127.0.0.1", "10.0.0.1"}
	for _, a := range outOfScope {
		err := s.CheckAddr(netip.MustParseAddr(a))
		var oos *ErrOutOfScope
		if !errors.As(err, &oos) {
			t.Errorf("CheckAddr(%s) = %v, want ErrOutOfScope", a, err)
		}
	}
}

func TestScopeCheckPrefixRequiresFullContainment(t *testing.T) {
	// Overlap is not enough. Allowing a sweep of 10.0.0.0/8 because
	// 10.20.30.0/24 is permitted would be exactly the failure this exists to
	// prevent — and an /8 sweep from an edge box is also a very effective way
	// to take out an OT link.
	s := mustScope(t, "10.20.30.0/24")

	if err := s.CheckPrefix(netip.MustParsePrefix("10.20.30.0/24")); err != nil {
		t.Errorf("the exact allowed range was refused: %v", err)
	}
	if err := s.CheckPrefix(netip.MustParsePrefix("10.20.30.128/25")); err != nil {
		t.Errorf("a range inside the allowed one was refused: %v", err)
	}

	wider := []string{"10.0.0.0/8", "10.20.0.0/16", "0.0.0.0/0"}
	for _, w := range wider {
		if err := s.CheckPrefix(netip.MustParsePrefix(w)); err == nil {
			t.Errorf("CheckPrefix(%s) was allowed by a /24 scope", w)
		}
	}

	if err := s.CheckPrefix(netip.MustParsePrefix("10.20.31.0/24")); err == nil {
		t.Error("an adjacent range outside the scope was allowed")
	}
}

func TestHostsExcludesNetworkAndBroadcast(t *testing.T) {
	hosts, err := Hosts(netip.MustParsePrefix("10.20.30.0/29"))
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	// A /29 is 8 addresses; 6 are usable.
	if len(hosts) != 6 {
		t.Fatalf("got %d hosts, want 6: %v", len(hosts), hosts)
	}
	if hosts[0].String() != "10.20.30.1" {
		t.Errorf("first host = %s, want 10.20.30.1 (network address not skipped)", hosts[0])
	}
	if hosts[len(hosts)-1].String() != "10.20.30.6" {
		t.Errorf("last host = %s, want 10.20.30.6 (broadcast address not skipped)", hosts[len(hosts)-1])
	}
}

func TestHostsPointToPointKeepsBothAddresses(t *testing.T) {
	// A /31 is a point-to-point link (RFC 3021) and has no network or broadcast
	// address to skip. Applying the usual rule would leave zero hosts.
	hosts, err := Hosts(netip.MustParsePrefix("10.20.30.0/31"))
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Errorf("a /31 produced %d hosts, want 2", len(hosts))
	}

	single, err := Hosts(netip.MustParsePrefix("10.20.30.7/32"))
	if err != nil {
		t.Fatalf("Hosts(/32): %v", err)
	}
	if len(single) != 1 || single[0].String() != "10.20.30.7" {
		t.Errorf("a /32 produced %v, want one address", single)
	}
}

func TestHostsRefusesOversizedRange(t *testing.T) {
	// A /16 is 65,534 addresses. Letting one node enqueue that many probes
	// would saturate a 100Mb OT link shared with the traffic that matters.
	_, err := Hosts(netip.MustParsePrefix("10.20.0.0/16"))
	if err == nil {
		t.Fatal("a /16 sweep was allowed")
	}
	if _, err := Hosts(netip.MustParsePrefix("10.20.0.0/20")); err != nil {
		t.Errorf("a /20 (4094 hosts) was refused: %v", err)
	}
}

func TestHostsRejectsIPv6(t *testing.T) {
	// Enumerating an IPv6 prefix is not meaningful — the smallest normal
	// allocation is larger than the number of addresses in IPv4 — so it is
	// refused rather than silently truncated.
	if _, err := Hosts(netip.MustParsePrefix("fd00::/64")); err == nil {
		t.Error("an IPv6 sweep was accepted")
	}
}

func TestCollectorMergesFindings(t *testing.T) {
	// Several probe types finding the same device must produce one entry, or an
	// inventory of forty machines reads as a hundred and sixty.
	c := NewCollector()
	c.Add(Device{Address: "10.20.30.5", Ports: []int{502}, Source: "port-scan",
		Details: map[string]any{"modbus": true}})
	c.Add(Device{Address: "10.20.30.5", Ports: []int{80}, Source: "port-scan",
		Details: map[string]any{"http": true}})
	c.Add(Device{Address: "10.20.30.5", Hostname: "press01.plant", Source: "rdns"})
	c.Add(Device{Address: "10.20.30.5", Protocol: "modbus",
		Details: map[string]any{"vendor": "Schneider"}})

	devices := c.Devices()
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devices), devices)
	}
	d := devices[0]
	if len(d.Ports) != 2 || d.Ports[0] != 80 || d.Ports[1] != 502 {
		t.Errorf("ports = %v, want [80 502] merged and sorted", d.Ports)
	}
	if d.Hostname != "press01.plant" {
		t.Errorf("hostname = %q", d.Hostname)
	}
	if d.Protocol != "modbus" {
		t.Errorf("protocol = %q", d.Protocol)
	}
	if d.Details["vendor"] != "Schneider" || d.Details["modbus"] != true || d.Details["http"] != true {
		t.Errorf("details = %v, want all three merged", d.Details)
	}
}

func TestCollectorSortsAddressesNumerically(t *testing.T) {
	// Lexical order puts .10 before .9, which reads as broken to anyone
	// scanning a device list for a machine they know the address of.
	c := NewCollector()
	for _, a := range []string{"10.20.30.10", "10.20.30.9", "10.20.30.100", "10.20.30.2"} {
		c.Add(Device{Address: a, Source: "test"})
	}
	got := c.Devices()
	want := []string{"10.20.30.2", "10.20.30.9", "10.20.30.10", "10.20.30.100"}
	for i := range want {
		if got[i].Address != want[i] {
			t.Errorf("device %d = %s, want %s", i, got[i].Address, want[i])
		}
	}
}

func TestCollectorOutputIsStableAcrossScans(t *testing.T) {
	// An unchanged network must produce identical output on repeated scans, or
	// a downstream filter node fires on nothing and an operator gets paged for
	// a change that did not happen.
	build := func() []Device {
		c := NewCollector()
		c.Add(Device{Address: "10.20.30.5", Ports: []int{502, 80}, Source: "s"})
		c.Add(Device{Address: "10.20.30.6", Ports: []int{80, 502}, Source: "s"})
		return c.Devices()
	}
	a, b := build(), build()
	if len(a) != len(b) {
		t.Fatalf("scan sizes differ: %d and %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Address != b[i].Address {
			t.Errorf("device %d differs between scans: %s and %s", i, a[i].Address, b[i].Address)
		}
		for j := range a[i].Ports {
			if a[i].Ports[j] != b[i].Ports[j] {
				t.Errorf("ports differ between scans for %s: %v and %v",
					a[i].Address, a[i].Ports, b[i].Ports)
			}
		}
	}
}

func TestSweepRefusesOutOfScope(t *testing.T) {
	// The check has to happen before any packet leaves, not after.
	s := mustScope(t, "10.20.30.0/24")
	_, err := Sweep(nil, s, netip.MustParsePrefix("192.168.99.0/24"), Options{})
	var oos *ErrOutOfScope
	if !errors.As(err, &oos) {
		t.Errorf("Sweep out of scope = %v, want ErrOutOfScope", err)
	}
}

func TestPortNamesAreReadable(t *testing.T) {
	// An operator reading a device list should see what the port is, not have
	// to remember that 44818 is EtherNet/IP.
	cases := map[int]string{
		502: "modbus", 4840: "opc-ua", 44818: "ethernet-ip",
		47808: "bacnet", 102: "s7", 9600: "omron-fins", 12345: "tcp/12345",
	}
	for port, want := range cases {
		if got := PortName(port); got != want {
			t.Errorf("PortName(%d) = %q, want %q", port, got, want)
		}
	}
}

func TestParseModbusDeviceID(t *testing.T) {
	// A real Read Device Identification response: MBAP header, function 43,
	// MEI 14, then three objects — vendor, product code, revision.
	resp := []byte{
		0x00, 0x01, 0x00, 0x00, 0x00, 0x1D, 0x01, // MBAP
		0x2B, 0x0E, 0x01, // function, MEI, read device id
		0x01, 0x00, 0x00, // conformity, more follows, next object id
		0x03,                                                    // object count
		0x00, 0x09, 'S', 'c', 'h', 'n', 'e', 'i', 'd', 'e', 'r', // vendor
		0x01, 0x05, 'M', '2', '5', '1', ' ', // product code
		0x02, 0x03, '2', '.', '1', // revision
	}
	got := parseModbusDeviceID(resp)
	if got["vendor"] != "Schneider" {
		t.Errorf("vendor = %#v, want Schneider", got["vendor"])
	}
	if got["productCode"] != "M251 " {
		t.Errorf("productCode = %#v", got["productCode"])
	}
	if got["revision"] != "2.1" {
		t.Errorf("revision = %#v", got["revision"])
	}
}

func TestParseModbusDeviceIDHandlesTruncatedResponse(t *testing.T) {
	// A device that closes the connection mid-response must not panic the
	// scanner — the whole point is to survive contact with whatever is on the
	// network.
	full := []byte{
		0x00, 0x01, 0x00, 0x00, 0x00, 0x1D, 0x01,
		0x2B, 0x0E, 0x01, 0x01, 0x00, 0x00, 0x03,
		0x00, 0x09, 'S', 'c', 'h', 'n', 'e', 'i', 'd', 'e', 'r',
	}
	for i := 0; i <= len(full); i++ {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("parseModbusDeviceID panicked on a %d-byte response: %v", i, p)
				}
			}()
			_ = parseModbusDeviceID(full[:i])
		}()
	}
}
