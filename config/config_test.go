package config

import (
	"strings"
	"testing"

	"github.com/Rakshit-gen/holdfast/bgp"
	"github.com/Rakshit-gen/holdfast/guard"
)

func TestExampleLoads(t *testing.T) {
	c, err := Load("../examples/holdfast.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Site != "ams1" || len(c.Services) != 2 || len(c.Fleet.Peers) != 3 {
		t.Fatalf("%+v", c)
	}
	s := c.Session(c.Neighbors[0])
	if s.Peer.String() != "192.0.2.1:179" || s.HoldTime.Seconds() != 90 || s.PeerAS != 64513 {
		t.Fatalf("session %+v", s)
	}
	p := c.Probe(c.Services[0])
	// The site drain file comes first, then the service's own checks.
	if !strings.HasPrefix(p.Check.String(), "absent /etc/holdfast/drain, dns health.example.com") || p.Damping == nil {
		t.Fatalf("probe %v", p.Check)
	}
	// dns sets its own floor; web uses the fleet's.
	if dns, web := c.Policy(c.Services[0]).Floor(4), c.Policy(c.Services[1]).Floor(4); dns != 3 || web != 2 {
		t.Fatalf("floors %d and %d", dns, web)
	}
}

func TestRoutesPerMode(t *testing.T) {
	c, _ := Load("../examples/holdfast.json")
	dns := c.Services[0]
	if rs := c.Routes(dns, guard.Withdraw); rs != nil {
		t.Fatalf("withdrawn service has routes %v", rs)
	}
	up := c.Routes(dns, guard.Announce)
	if len(up) != 2 || up[0].Attrs.PathLen() != 0 || bgp.CommunityString(up[0].Attrs.Communities[0]) != "64512:53" {
		t.Fatalf("announce %+v", up)
	}
	down := c.Routes(dns, guard.Degrade)
	if down[0].Attrs.PathString() != "64512 64512 64512" {
		t.Fatalf("degrade path %q", down[0].Attrs.PathString())
	}
	// Changing one route's path mustn't change another's.
	down[0].Attrs.Prepend(1, 1)
	if down[1].Attrs.PathLen() != 3 {
		t.Fatal("routes share a path")
	}
}

func TestProblemsAreAllReported(t *testing.T) {
	_, err := Parse([]byte(`{
		"site": "a", "asn": 64512, "router_id": "192.0.2.1", "api": "0.0.0.0:9179", "graceful_shutdown": "1h",
		"neighbors": [{"address": "192.0.2.2", "asn": 64513, "hold_time": "1s", "passive": true}],
		"services": [
			{"name": "dns", "prefixes": ["198.51.100.1/24"], "checks": [{"type": "ping"}]},
			{"name": "web", "prefixes": ["198.51.100.0/24"], "checks": []}
		],
		"fleet": {"min_up": 5, "peers": [{"site": "a", "url": "http://x"}]}
	}`))
	if err == nil {
		t.Fatal("bad config accepted")
	}
	for _, want := range []string{
		"hold_time must be 0 or at least 3s",
		"passive but listen isn't set",
		"has host bits set; did you mean 198.51.100.0/24",
		"is also in service dns",
		"next_hop isn't set",
		`type "ping"`,
		"no checks",
		"appears twice (or is this site)",
		"min_up is 5 but there are only 2 sites",
		"fleet.listen is required",
		"api must listen on a loopback address",
		"graceful_shutdown must be 0 to 5m",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestUnknownFieldsAreErrors(t *testing.T) {
	_, err := Parse([]byte(`{"site": "a", "fleet": {"minup": 3}}`))
	if err == nil || !strings.Contains(err.Error(), `unknown field "minup"`) {
		t.Fatalf("got %v", err)
	}
}

func TestSyntaxErrorsHaveLines(t *testing.T) {
	_, err := Parse([]byte("{\n\"site\": \"a\",\n}"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("got %v", err)
	}
}

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9179": true, "[::1]:9179": true, "localhost:9179": true,
		"0.0.0.0:9179": false, ":9179": false, "192.0.2.1:9179": false, "127.0.0.1": false,
	} {
		if got := loopback(addr); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}
