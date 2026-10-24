// Package config reads holdfast's JSON config file and checks it.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/Rakshit-gen/holdfast/bgp"
)

// Duration is a time.Duration written as "2s" or "1m30s".
type Duration time.Duration

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("want a duration like \"5s\", got %s", b)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes it back the same way.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Config is the whole file.
type Config struct {
	// Site names this holdfast to the rest of the fleet.
	Site     string     `json:"site"`
	ASN      uint32     `json:"asn"`
	RouterID netip.Addr `json:"router_id"`
	NextHop  netip.Addr `json:"next_hop"`
	NextHop6 netip.Addr `json:"next_hop6"`
	// Listen accepts connections from passive neighbors, like ":179".
	Listen string `json:"listen"`
	// API serves status and drain on this address; keep it on loopback.
	API string `json:"api"`
	// DrainFile, if it exists, drains every service at this site.
	DrainFile string     `json:"drain_file"`
	Neighbors []Neighbor `json:"neighbors"`
	Services  []Service  `json:"services"`
	Fleet     Fleet      `json:"fleet"`
}

// Neighbor is one BGP peer.
type Neighbor struct {
	Address      netip.Addr `json:"address"`
	Port         uint16     `json:"port"`
	ASN          uint32     `json:"asn"`
	Passive      bool       `json:"passive"`
	LocalAddress netip.Addr `json:"local_address"`
	HoldTime     Duration   `json:"hold_time"`
	Families     []string   `json:"families"` // "ipv4", "ipv6"
	MaxPrefixes  int        `json:"max_prefixes"`
	// GracefulRestart offers RFC 4724 with this restart time. Leave it
	// off unless the peer should keep routing to this site while holdfast
	// is down.
	GracefulRestart Duration `json:"graceful_restart"`
}

// Service is a set of prefixes announced together, and the checks that
// decide whether to.
type Service struct {
	Name             string   `json:"name"`
	Prefixes         []string `json:"prefixes"`
	Communities      []string `json:"communities"`
	LargeCommunities []string `json:"large_communities"`
	MED              *uint32  `json:"med"`
	// Degrade is what a sick site that may not withdraw announces: the
	// path prepended this many more times (default 3) and these extra
	// communities.
	DegradePrepend     *int     `json:"degrade_prepend"`
	DegradeCommunities []string `json:"degrade_communities"`

	Checks   []Check  `json:"checks"`
	Interval Duration `json:"interval"`
	Timeout  Duration `json:"timeout"`
	Rise     int      `json:"rise"`
	Fall     int      `json:"fall"`
	Damping  bool     `json:"damping"`
}

// Check is one health check. Type picks which fields apply.
type Check struct {
	Type     string   `json:"type"` // tcp, http, dns, exec, absent
	Addr     string   `json:"addr"`
	URL      string   `json:"url"`
	Host     string   `json:"host"`
	Status   int      `json:"status"`
	Contains string   `json:"contains"`
	Server   string   `json:"server"`
	Name     string   `json:"name"`
	Command  []string `json:"command"`
	Path     string   `json:"path"`
}

// Fleet is the other sites and the guard's policy.
type Fleet struct {
	Listen    string   `json:"listen"`
	TokenFile string   `json:"token_file"`
	Peers     []Peer   `json:"peers"`
	Interval  Duration `json:"interval"`
	// MinUp and MinFraction set how many sites must keep announcing each
	// service; the larger wins.
	MinUp       int      `json:"min_up"`
	MinFraction float64  `json:"min_fraction"`
	Stale       Duration `json:"stale"`
	Quorum      float64  `json:"quorum"`
	// Unguarded withdraws on any failed check, like a plain health
	// checker. Only for comparison.
	Unguarded bool `json:"unguarded"`
}

// Peer is another site.
type Peer struct {
	Site string `json:"site"`
	URL  string `json:"url"`
}

// Load reads and checks a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse reads and checks a config. Unknown fields are errors, since a
// misspelled "min_up" silently falling back to the default is exactly the
// kind of mistake this tool exists to stop.
func Parse(b []byte) (*Config, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var c Config
	if err := d.Decode(&c); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			line := 1 + bytes.Count(b[:se.Offset], []byte("\n"))
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		return nil, err
	}
	if err := c.Check(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Check returns every problem with the config at once.
func (c *Config) Check() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if c.Site == "" {
		bad("site is required")
	}
	if c.ASN == 0 {
		bad("asn is required")
	}
	if !c.RouterID.Is4() {
		bad("router_id must be an IPv4 address")
	}
	if c.NextHop.IsValid() && !c.NextHop.Is4() {
		bad("next_hop must be IPv4; use next_hop6 for IPv6")
	}
	if c.NextHop6.IsValid() && !c.NextHop6.Is6() {
		bad("next_hop6 must be IPv6")
	}
	if len(c.Neighbors) == 0 {
		bad("no neighbors")
	}
	passive := false
	seenN := map[netip.Addr]bool{}
	for i, n := range c.Neighbors {
		at := fmt.Sprintf("neighbors[%d]", i)
		if !n.Address.IsValid() {
			bad("%s: address is required", at)
		}
		if seenN[n.Address] {
			bad("%s: %v is listed twice", at, n.Address)
		}
		seenN[n.Address] = true
		if n.ASN == 0 {
			bad("%s: asn is required", at)
		}
		hold := time.Duration(n.HoldTime)
		if hold != 0 && hold < 3*time.Second {
			bad("%s: hold_time must be 0 or at least 3s", at)
		}
		for _, f := range n.Families {
			if f != "ipv4" && f != "ipv6" {
				bad("%s: family %q, want ipv4 or ipv6", at, f)
			}
		}
		passive = passive || n.Passive
	}
	if passive && c.Listen == "" {
		bad("a neighbor is passive but listen isn't set")
	}

	if len(c.Services) == 0 {
		bad("no services")
	}
	seenS := map[string]bool{}
	owner := map[netip.Prefix]string{}
	for i, s := range c.Services {
		at := fmt.Sprintf("services[%d]", i)
		if s.Name == "" {
			bad("%s: name is required", at)
		} else {
			at = "service " + s.Name
		}
		if seenS[s.Name] {
			bad("%s: listed twice", at)
		}
		seenS[s.Name] = true
		if len(s.Prefixes) == 0 {
			bad("%s: no prefixes", at)
		}
		for _, p := range s.Prefixes {
			pf, err := netip.ParsePrefix(p)
			if err != nil {
				bad("%s: %v", at, err)
				continue
			}
			if pf != pf.Masked() {
				bad("%s: %s has host bits set; did you mean %s?", at, p, pf.Masked())
			}
			if o, ok := owner[pf.Masked()]; ok {
				bad("%s: %s is also in service %s", at, p, o)
			}
			owner[pf.Masked()] = s.Name
			if pf.Addr().Is4() && !c.NextHop.IsValid() {
				bad("%s: %s is IPv4 but next_hop isn't set", at, p)
			}
			if pf.Addr().Is6() && !c.NextHop6.IsValid() {
				bad("%s: %s is IPv6 but next_hop6 isn't set", at, p)
			}
		}
		for _, cm := range append(append([]string{}, s.Communities...), s.DegradeCommunities...) {
			if _, err := bgp.ParseCommunity(cm); err != nil {
				bad("%s: %v", at, err)
			}
		}
		for _, cm := range s.LargeCommunities {
			if _, err := bgp.ParseLargeCommunity(cm); err != nil {
				bad("%s: %v", at, err)
			}
		}
		if s.DegradePrepend != nil && (*s.DegradePrepend < 0 || *s.DegradePrepend > 10) {
			bad("%s: degrade_prepend must be 0 to 10", at)
		}
		if len(s.Checks) == 0 {
			bad("%s: no checks; a service with no checks would never withdraw", at)
		}
		for j, ch := range s.Checks {
			if err := ch.check(); err != nil {
				bad("%s: checks[%d]: %v", at, j, err)
			}
		}
		if s.Rise < 0 || s.Fall < 0 {
			bad("%s: rise and fall can't be negative", at)
		}
	}

	f := c.Fleet
	seenP := map[string]bool{c.Site: true}
	for i, p := range f.Peers {
		if p.Site == "" || p.URL == "" {
			bad("fleet.peers[%d]: site and url are required", i)
		}
		if seenP[p.Site] {
			bad("fleet.peers[%d]: site %q appears twice (or is this site)", i, p.Site)
		}
		seenP[p.Site] = true
	}
	if len(f.Peers) > 0 && f.Listen == "" {
		bad("fleet.listen is required when there are peers, so they can see this site")
	}
	if f.MinFraction < 0 || f.MinFraction > 1 {
		bad("fleet.min_fraction must be 0 to 1")
	}
	if f.Quorum < 0 || f.Quorum > 1 {
		bad("fleet.quorum must be 0 to 1")
	}
	if n := len(f.Peers) + 1; f.MinUp > n {
		bad("fleet.min_up is %d but there are only %d sites", f.MinUp, n)
	}
	// A site refreshes its own report once a second, so anything under a
	// couple of seconds marks healthy sites unknown between refreshes.
	if f.Stale > 0 && time.Duration(f.Stale) < 2*time.Second {
		bad("fleet.stale must be at least 2s")
	}
	if f.Interval > 0 && f.Stale > 0 && f.Stale < 2*f.Interval {
		bad("fleet.stale (%v) should be at least twice fleet.interval (%v), or one slow poll marks a site unknown",
			time.Duration(f.Stale), time.Duration(f.Interval))
	}
	return errors.Join(errs...)
}

func (ch Check) check() error {
	need := func(name, v string) error {
		if v == "" {
			return fmt.Errorf("%s check needs %s", ch.Type, name)
		}
		return nil
	}
	switch ch.Type {
	case "tcp":
		return need("addr", ch.Addr)
	case "http":
		if !strings.HasPrefix(ch.URL, "http://") && !strings.HasPrefix(ch.URL, "https://") {
			return fmt.Errorf("http check needs an http:// or https:// url")
		}
	case "dns":
		return errors.Join(need("server", ch.Server), need("name", ch.Name))
	case "exec":
		if len(ch.Command) == 0 {
			return fmt.Errorf("exec check needs command, as a list of arguments")
		}
	case "absent":
		return need("path", ch.Path)
	default:
		return fmt.Errorf("type %q, want tcp, http, dns, exec or absent", ch.Type)
	}
	return nil
}
