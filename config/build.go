package config

import (
	"net/netip"
	"time"

	"github.com/Rakshit-gen/holdfast/bgp"
	"github.com/Rakshit-gen/holdfast/guard"
	"github.com/Rakshit-gen/holdfast/health"
)

// Session turns a neighbor into a session config.
func (c *Config) Session(n Neighbor) bgp.Config {
	port := n.Port
	if port == 0 {
		port = 179
	}
	out := bgp.Config{
		LocalAS: c.ASN, PeerAS: n.ASN, RouterID: c.RouterID,
		Peer: netip.AddrPortFrom(n.Address, port), Passive: n.Passive,
		LocalAddr: n.LocalAddress, HoldTime: time.Duration(n.HoldTime),
		NextHop: c.NextHop, NextHop6: c.NextHop6,
		MaxPrefixes: n.MaxPrefixes, GracefulRestart: time.Duration(n.GracefulRestart),
	}
	for _, f := range n.Families {
		if f == "ipv4" {
			out.Families = append(out.Families, bgp.IPv4Unicast)
		} else {
			out.Families = append(out.Families, bgp.IPv6Unicast)
		}
	}
	return out
}

// Probe builds the health probe for a service: its checks, plus the
// site-wide drain file if there is one, all of which must pass.
func (c *Config) Probe(s Service) health.Probe {
	var all health.All
	if c.DrainFile != "" {
		all = append(all, health.Absent{Path: c.DrainFile})
	}
	for _, ch := range s.Checks {
		all = append(all, ch.Build())
	}
	p := health.Probe{
		Name: s.Name, Check: all, Interval: time.Duration(s.Interval), Timeout: time.Duration(s.Timeout),
		Rise: s.Rise, Fall: s.Fall,
	}
	if s.Damping {
		d := health.DefaultDamping
		p.Damping = &d
	}
	return p
}

// Build makes the check.
func (ch Check) Build() health.Check {
	switch ch.Type {
	case "tcp":
		return health.TCP{Addr: ch.Addr}
	case "http":
		return health.HTTP{URL: ch.URL, Host: ch.Host, Status: ch.Status, Contains: ch.Contains}
	case "dns":
		return health.DNS{Server: ch.Server, Name: ch.Name}
	case "exec":
		return health.Exec{Argv: ch.Command}
	}
	return health.Absent{Path: ch.Path}
}

// Routes is what a service announces in a mode: nothing when withdrawn,
// and a longer path plus the degrade communities when degraded.
func (c *Config) Routes(s Service, mode guard.Mode) []bgp.Route {
	if mode == guard.Withdraw {
		return nil
	}
	var a bgp.Attrs
	a.Origin = bgp.OriginIGP
	for _, cm := range s.Communities {
		v, _ := bgp.ParseCommunity(cm)
		a.Communities = append(a.Communities, v)
	}
	for _, cm := range s.LargeCommunities {
		v, _ := bgp.ParseLargeCommunity(cm)
		a.LargeCommunities = append(a.LargeCommunities, v)
	}
	if s.MED != nil {
		a.MED, a.HasMED = *s.MED, true
	}
	if mode == guard.Degrade {
		n := 3
		if s.DegradePrepend != nil {
			n = *s.DegradePrepend
		}
		a.Prepend(c.ASN, n)
		for _, cm := range s.DegradeCommunities {
			v, _ := bgp.ParseCommunity(cm)
			a.Communities = append(a.Communities, v)
		}
	}
	var out []bgp.Route
	for _, p := range s.Prefixes {
		pf, _ := netip.ParsePrefix(p)
		r := bgp.Route{Prefix: pf.Masked(), Attrs: a}
		// Each route gets its own copy of the path and communities.
		r.Attrs.ASPath = append([]bgp.Segment(nil), a.ASPath...)
		r.Attrs.Communities = append([]uint32(nil), a.Communities...)
		out = append(out, r)
	}
	return out
}

// Policy is the guard policy.
func (c *Config) Policy() guard.Policy {
	f := c.Fleet
	return guard.Policy{
		MinUp: f.MinUp, MinFraction: f.MinFraction, Stale: time.Duration(f.Stale),
		Quorum: f.Quorum, Unguarded: f.Unguarded,
	}
}
