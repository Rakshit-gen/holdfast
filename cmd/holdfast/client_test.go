package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rakshit-gen/holdfast/daemon"
)

func TestPrintStatus(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := daemon.Status{
		Site: "ams1",
		Services: []daemon.ServiceStatus{{
			Name: "dns", Mode: "degrade", Holding: true, Reason: "unhealthy, but withdrawing would leave too few sites",
			LastErr: "dns: timeout", Since: now.Add(-90 * time.Second),
			Sites: []daemon.SiteStatus{{Site: "ams1", Mode: "degrade"}, {Site: "fra1", Mode: "withdraw"}, {Site: "lhr1"}},
		}},
		Neighbors: []daemon.NeighborStatus{{Address: "192.0.2.1", State: "established", Sent: 2}},
		Fleet:     map[string]string{"lhr1": "timeout", "cdg1": "refused"},
	}
	var b strings.Builder
	printStatus(&b, st, now)
	out := b.String()
	for _, want := range []string{"site ams1", "degrade (held)", "1m30s", "2/3 announcing", "too few sites: dns: timeout", "192.0.2.1", "cdg1: refused\n  lhr1: timeout"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestPrintFleet(t *testing.T) {
	st := daemon.Status{Site: "ams1", Services: []daemon.ServiceStatus{{
		Name:  "dns",
		Sites: []daemon.SiteStatus{{Site: "ams1", Healthy: true, Mode: "announce"}, {Site: "fra1", Mode: "withdraw", Age: 4.2}},
	}}}
	var b strings.Builder
	printFleet(&b, st)
	if out := b.String(); !strings.Contains(out, "dns, as ams1 sees the fleet") || !strings.Contains(out, "fra1  no       withdraw  4s") {
		t.Fatalf("got\n%s", out)
	}
}

func TestDrain(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.String()
		if r.URL.Query().Get("service") == "nope" {
			http.Error(w, "no service nope", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	api := strings.TrimPrefix(srv.URL, "http://")
	if err := drainCmd(true)([]string{"-api", api, "dns"}); err != nil || got != "POST /v1/drain?service=dns" {
		t.Fatal(err, got)
	}
	if err := drainCmd(false)([]string{"-api", api}); err != nil || got != "POST /v1/undrain" {
		t.Fatal(err, got)
	}
	if err := drainCmd(true)([]string{"-api", api, "nope"}); err == nil || !strings.Contains(err.Error(), "no service nope") {
		t.Fatal(err)
	}
}
