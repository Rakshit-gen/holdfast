package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rakshit-gen/holdfast/guard"
)

// three makes sites a, b and c, each serving and polling the others.
func three(t *testing.T, token string) map[string]*Node {
	nodes := map[string]*Node{}
	urls := map[string]string{}
	for _, s := range []string{"a", "b", "c"} {
		n := &Node{Site: s, Token: token, Interval: 20 * time.Millisecond}
		srv := httptest.NewServer(n)
		t.Cleanup(srv.Close)
		nodes[s], urls[s] = n, srv.URL
	}
	for s, n := range nodes {
		for o, u := range urls {
			if o != s {
				n.Peers = append(n.Peers, Peer{Site: o, URL: u})
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, n := range nodes {
		go n.Run(ctx)
	}
	return nodes
}

func wait(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); !f(); {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSitesSeeEachOther(t *testing.T) {
	ns := three(t, "s3cret")
	ns["a"].Set("dns", true, guard.Announce)
	ns["b"].Set("dns", false, guard.Degrade)
	ns["c"].Set("dns", true, guard.Announce)
	wait(t, "a full view at c", func() bool { return len(ns["c"].View("dns")) == 3 })
	for _, r := range ns["c"].View("dns") {
		if r.Site == "b" && (r.Healthy || r.Mode != guard.Degrade) {
			t.Fatalf("b reported as %+v", r)
		}
		if time.Since(r.At) > time.Second {
			t.Fatalf("%s report is %v old", r.Site, time.Since(r.At))
		}
	}
	if len(ns["a"].View("web")) != 0 {
		t.Fatal("a view of a service nobody has")
	}
	if got := ns["a"].Sites(); len(got) != 3 || got[0] != "a" {
		t.Fatalf("sites %v", got)
	}
}

func TestTokenRequired(t *testing.T) {
	n := &Node{Site: "a", Token: "s3cret"}
	srv := httptest.NewServer(n)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/v1/reports")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	other := &Node{Site: "b", Token: "wrong", Peers: []Peer{{"a", srv.URL}}}
	other.poll(context.Background(), other.Peers[0])
	if !strings.Contains(other.PeerErrors()["a"], "401") {
		t.Fatalf("errors %v", other.PeerErrors())
	}
}

func TestWrongSiteAnswering(t *testing.T) {
	srv := httptest.NewServer(&Node{Site: "imposter"})
	defer srv.Close()
	n := &Node{Site: "b", Peers: []Peer{{"a", srv.URL}}}
	n.poll(context.Background(), n.Peers[0])
	if !strings.Contains(n.PeerErrors()["a"], "imposter") {
		t.Fatalf("errors %v", n.PeerErrors())
	}
}

func TestDeadPeerGoesStale(t *testing.T) {
	a := &Node{Site: "a"}
	a.Set("dns", true, guard.Announce)
	srv := httptest.NewServer(a)
	b := &Node{Site: "b", Peers: []Peer{{"a", srv.URL}}, Client: &http.Client{Timeout: time.Second}}
	b.poll(context.Background(), b.Peers[0])
	at := b.View("dns")[0].At
	srv.Close()
	b.poll(context.Background(), b.Peers[0])
	// The old report stays, with its old time, for the guard to judge.
	if v := b.View("dns"); len(v) != 1 || !v[0].At.Equal(at) {
		t.Fatalf("view %+v", v)
	}
	if b.PeerErrors()["a"] == "" {
		t.Fatal("no error recorded")
	}
}

func TestAgeIsCountedFromTheRequest(t *testing.T) {
	// A site that answers with an old report shows up as old, whatever
	// its clock says.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"site":"a","services":[{"service":"dns","healthy":true,"mode":"announce","age_seconds":120}]}`))
	}))
	defer srv.Close()
	b := &Node{Site: "b", Peers: []Peer{{"a", srv.URL}}, Client: http.DefaultClient}
	b.poll(context.Background(), b.Peers[0])
	age := time.Since(b.View("dns")[0].At)
	if age < 119*time.Second || age > 125*time.Second {
		t.Fatalf("age %v", age)
	}
}

func TestBadReportsAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"unknown mode": `{"site":"a","services":[{"service":"dns","mode":"sideways"}]}`,
		"negative age": `{"site":"a","services":[{"service":"dns","mode":"announce","age_seconds":-5}]}`,
		"not json":     `<html>`,
		"too big":      `{"site":"a","services":[` + strings.Repeat(`{"service":"x","mode":"announce"},`, 40000) + `]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))
		b := &Node{Site: "b", Peers: []Peer{{"a", srv.URL}}, Client: http.DefaultClient}
		b.poll(context.Background(), b.Peers[0])
		srv.Close()
		if b.PeerErrors()["a"] == "" || len(b.View("dns")) != 0 {
			t.Errorf("%s: accepted, view %v", name, b.View("dns"))
		}
	}
}

func TestUnchangedSetStaysCurrent(t *testing.T) {
	// The guard counts a report as fresh by when it was last confirmed,
	// not when it last changed.
	a := &Node{Site: "a"}
	a.Set("dns", true, guard.Announce)
	first := a.View("dns")[0].At
	time.Sleep(10 * time.Millisecond)
	a.Set("dns", true, guard.Announce)
	if !a.View("dns")[0].At.After(first) {
		t.Fatal("time didn't move")
	}
}
