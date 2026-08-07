// Package discover implements network discovery for the scan nodes.
//
// This is a plant-floor inventory tool. Everything in it is bounded by an
// operator-configured CIDR allowlist, because the nodes are reachable by anyone
// who can edit a flow, and "it is on an isolated network" is an assumption that
// stops being true the day somebody bridges a VLAN.
package discover

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
)

// Scope decides what the scan nodes are permitted to probe.
type Scope struct {
	enabled bool
	allowed []netip.Prefix
}

// NewScope builds a scope from the configured CIDRs.
//
// An enabled scope with no CIDRs is an error rather than "everything". The
// permissive reading of an empty list is how a tool meant to inventory one
// factory segment ends up scanning a corporate network.
func NewScope(enabled bool, cidrs []string) (*Scope, error) {
	s := &Scope{enabled: enabled}
	if !enabled {
		return s, nil
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("discovery is enabled but no CIDRs are allowed; " +
			"list the networks the scan nodes may probe")
	}
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return nil, fmt.Errorf("allowed CIDR %q: %w", c, err)
		}
		// Masked so that 10.20.30.5/24 behaves as 10.20.30.0/24 rather than
		// silently never matching.
		s.allowed = append(s.allowed, p.Masked())
	}
	return s, nil
}

// Enabled reports whether discovery is switched on at all.
func (s *Scope) Enabled() bool { return s != nil && s.enabled }

// ErrOutOfScope is returned for a target outside the allowlist.
type ErrOutOfScope struct{ Target string }

func (e *ErrOutOfScope) Error() string {
	return fmt.Sprintf("%s is outside the configured discovery scope", e.Target)
}

// ErrDisabled is returned when discovery is off.
var ErrDisabled = fmt.Errorf("discovery is disabled; enable it and list the " +
	"networks the scan nodes may probe")

// CheckAddr reports whether a single address may be probed.
func (s *Scope) CheckAddr(addr netip.Addr) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	for _, p := range s.allowed {
		if p.Contains(addr) {
			return nil
		}
	}
	return &ErrOutOfScope{Target: addr.String()}
}

// CheckHost resolves a host and reports whether every address it resolves to is
// in scope.
//
// Every address, not any: a name that resolves to one in-scope and one
// out-of-scope address must be refused, or DNS becomes the way around the
// allowlist.
func (s *Scope) CheckHost(host string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return s.CheckAddr(addr)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%s did not resolve to any address", host)
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			return fmt.Errorf("%s resolved to an unusable address", host)
		}
		if err := s.CheckAddr(addr.Unmap()); err != nil {
			return err
		}
	}
	return nil
}

// CheckPrefix reports whether an entire range may be swept.
//
// The range must be fully contained in an allowed prefix. Overlap is not
// enough: sweeping 10.0.0.0/8 because 10.20.30.0/24 is allowed would be exactly
// the failure this exists to prevent.
func (s *Scope) CheckPrefix(p netip.Prefix) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	p = p.Masked()
	for _, a := range s.allowed {
		if a.Contains(p.Addr()) && p.Bits() >= a.Bits() {
			return nil
		}
	}
	return &ErrOutOfScope{Target: p.String()}
}

// MaxSweepHosts caps a single sweep.
//
// A /16 is 65,534 addresses. Allowing one node to enqueue that many probes
// would saturate an OT link that is often 100Mb and shared with the process
// traffic that actually matters.
const MaxSweepHosts = 4096

// Hosts expands a prefix into the addresses to probe, excluding the network and
// broadcast addresses for IPv4 prefixes shorter than /31.
func Hosts(p netip.Prefix) ([]netip.Addr, error) {
	p = p.Masked()
	if !p.Addr().Is4() {
		return nil, fmt.Errorf("sweeping %s: only IPv4 ranges are supported", p)
	}

	bits := p.Bits()
	total := 1 << uint(32-bits)
	if bits < 31 {
		total -= 2 // network and broadcast
	}
	if total > MaxSweepHosts {
		return nil, fmt.Errorf("%s covers %d addresses, more than the %d limit; use a smaller range",
			p, total, MaxSweepHosts)
	}

	out := make([]netip.Addr, 0, total)
	addr := p.Addr()
	if bits < 31 {
		addr = addr.Next() // skip the network address
	}
	for i := 0; i < total; i++ {
		if !p.Contains(addr) {
			break
		}
		out = append(out, addr)
		addr = addr.Next()
	}
	return out, nil
}

// Device is one thing found on the network.
type Device struct {
	Address  string            `json:"address"`
	MAC      string            `json:"mac,omitempty"`
	Hostname string            `json:"hostname,omitempty"`
	Vendor   string            `json:"vendor,omitempty"`
	Ports    []int             `json:"ports,omitempty"`
	Protocol string            `json:"protocol,omitempty"`
	Details  map[string]any    `json:"details,omitempty"`
	Source   string            `json:"source"`
	Extra    map[string]string `json:"extra,omitempty"`
}

// Collector accumulates results from concurrent probes.
type Collector struct {
	mu      sync.Mutex
	devices map[string]*Device
}

// NewCollector returns an empty collector.
func NewCollector() *Collector {
	return &Collector{devices: map[string]*Device{}}
}

// Add merges a finding, keyed by address, so that several probe types
// discovering the same device produce one entry rather than four.
func (c *Collector) Add(d Device) {
	c.mu.Lock()
	defer c.mu.Unlock()

	existing, ok := c.devices[d.Address]
	if !ok {
		cp := d
		c.devices[d.Address] = &cp
		return
	}
	if existing.MAC == "" {
		existing.MAC = d.MAC
	}
	if existing.Hostname == "" {
		existing.Hostname = d.Hostname
	}
	if existing.Vendor == "" {
		existing.Vendor = d.Vendor
	}
	if existing.Protocol == "" {
		existing.Protocol = d.Protocol
	}
	existing.Ports = mergePorts(existing.Ports, d.Ports)
	if d.Details != nil {
		if existing.Details == nil {
			existing.Details = map[string]any{}
		}
		for k, v := range d.Details {
			existing.Details[k] = v
		}
	}
}

func mergePorts(a, b []int) []int {
	seen := make(map[int]bool, len(a)+len(b))
	out := make([]int, 0, len(a)+len(b))
	for _, list := range [][]int{a, b} {
		for _, p := range list {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	// Sorted so repeated scans of an unchanged network produce identical output
	// and a downstream change-detection filter does not fire on nothing.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Devices returns the findings, sorted by address.
func (c *Collector) Devices() []Device {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]Device, 0, len(c.devices))
	for _, d := range c.devices {
		out = append(out, *d)
	}
	// Sorted numerically by address rather than lexically, so 10.20.30.9 comes
	// before 10.20.30.10 the way an operator reading a device list expects.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && lessAddr(out[j].Address, out[j-1].Address); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func lessAddr(a, b string) bool {
	aa, aerr := netip.ParseAddr(a)
	ba, berr := netip.ParseAddr(b)
	if aerr == nil && berr == nil {
		return aa.Less(ba)
	}
	return a < b
}
