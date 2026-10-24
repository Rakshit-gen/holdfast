package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var errDown = errors.New("down")

func TestTrackerRiseAndFall(t *testing.T) {
	now := time.Unix(0, 0)
	tr := newTracker(Probe{Rise: 2, Fall: 3}, now)
	steps := []struct {
		err  error
		up   bool
		flip bool
	}{
		{nil, false, false}, // one pass isn't enough
		{nil, true, true},
		{errDown, true, false},
		{errDown, true, false},
		{nil, true, false}, // the streak resets
		{errDown, true, false},
		{errDown, true, false},
		{errDown, false, true},
		{nil, false, false},
		{nil, true, true},
	}
	for i, s := range steps {
		now = now.Add(time.Second)
		flip := tr.observe(now, s.err)
		if tr.st.Up != s.up || flip != s.flip {
			t.Fatalf("step %d: up %v flip %v, want %v %v", i, tr.st.Up, flip, s.up, s.flip)
		}
	}
	if tr.st.LastErr != "down" || tr.st.Checks != len(steps) {
		t.Fatalf("status %+v", tr.st)
	}
}

func TestDampingHoldsAFlappingCheck(t *testing.T) {
	now := time.Unix(0, 0)
	d := DefaultDamping
	tr := newTracker(Probe{Rise: 1, Fall: 1, Damping: &d}, now)
	step := func(err error) bool {
		now = now.Add(time.Second)
		tr.observe(now, err)
		return tr.st.Up
	}
	// Two flaps: penalty about 2000, under the 2500 suppress line.
	for range 2 {
		step(nil)
		step(errDown)
	}
	if !step(nil) {
		t.Fatal("held after two flaps")
	}
	step(errDown) // third flap
	if step(nil) || !tr.st.Held {
		t.Fatalf("not held after three flaps: %+v", tr.st)
	}
	until := tr.st.HeldUntil
	// About 7 minutes for ~3000 to decay to 750 with a 5 minute half life.
	if wait := until.Sub(now); wait < 9*time.Minute || wait > 11*time.Minute {
		t.Fatalf("held for %v", wait)
	}
	now = until.Add(time.Second)
	if !step(nil) || tr.st.Held {
		t.Fatalf("still held after release: %+v", tr.st)
	}
}

func TestDampingCap(t *testing.T) {
	m := &damper{d: DefaultDamping}
	now := time.Unix(0, 0)
	for range 100 {
		m.flap(now)
	}
	if m.penalty != DefaultDamping.Max {
		t.Fatalf("penalty %v", m.penalty)
	}
	// log2(12000/750) = 4 half lives.
	if got := m.until(now).Sub(now); got != 20*time.Minute {
		t.Fatalf("held for %v, want 20m", got)
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("status: serving"))
		case "/moved":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/host":
			if r.Host != "www.example.com" {
				w.WriteHeader(404)
			}
		default:
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	cases := []struct {
		c  HTTP
		ok bool
	}{
		{HTTP{URL: srv.URL + "/ok"}, true},
		{HTTP{URL: srv.URL + "/ok", Contains: "serving"}, true},
		{HTTP{URL: srv.URL + "/ok", Contains: "draining"}, false},
		{HTTP{URL: srv.URL + "/down"}, false},
		{HTTP{URL: srv.URL + "/moved"}, true}, // 302 counts, and isn't followed
		{HTTP{URL: srv.URL + "/moved", Status: 200}, false},
		{HTTP{URL: srv.URL + "/host", Host: "www.example.com"}, true},
		{HTTP{URL: srv.URL + "/host"}, false},
	}
	for _, c := range cases {
		if err := c.c.Check(ctx); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.c, err)
		}
	}
}

func TestTCP(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	if err := (TCP{addr}).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if err := (TCP{addr}).Check(context.Background()); err == nil {
		t.Fatal("closed port passed")
	}
}

func TestExecAndAbsent(t *testing.T) {
	ctx := context.Background()
	if err := (Exec{[]string{"true"}}).Check(ctx); err != nil {
		t.Fatal(err)
	}
	err := (Exec{[]string{"sh", "-c", "echo disk full; exit 1"}}).Check(ctx)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("got %v", err)
	}
	tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := (Exec{[]string{"sleep", "5"}}).Check(tctx); err == nil {
		t.Fatal("timeout passed")
	}

	f := filepath.Join(t.TempDir(), "drain")
	if err := (Absent{f}).Check(ctx); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(f, nil, 0o644)
	if err := (Absent{f}).Check(ctx); err == nil {
		t.Fatal("drain file ignored")
	}
}

// dnsServer answers every A query with 192.0.2.1, or nothing if empty.
func dnsServer(t *testing.T, empty bool) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			_, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			// The question is the name, then type and class. Go's resolver
			// adds an EDNS record after it, which this answer drops.
			end := 12
			for buf[end] != 0 {
				end += int(buf[end]) + 1
			}
			end += 5
			q := buf[:end]
			resp := append([]byte(nil), q[:2]...)
			resp = append(resp, 0x81, 0x80) // response, recursion available
			resp = append(resp, 0, 1)       // one question
			if empty || q[end-3] != 1 {       // AAAA etc: no answers
				resp = append(resp, 0, 0, 0, 0, 0, 0)
				resp = append(resp, q[12:]...)
			} else {
				resp = append(resp, 0, 1, 0, 0, 0, 0)
				resp = append(resp, q[12:]...)
				resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 1)
			}
			pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestDNS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := (DNS{Server: dnsServer(t, false), Name: "svc.example.com"}).Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := (DNS{Server: dnsServer(t, true), Name: "svc.example.com"}).Check(ctx); err == nil {
		t.Fatal("empty answer passed")
	}
}

type flaky struct {
	mu   sync.Mutex
	fail bool
}

func (f *flaky) Check(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errDown
	}
	return nil
}
func (f *flaky) String() string { return "flaky" }

func TestProberReportsChanges(t *testing.T) {
	f := &flaky{}
	changes := make(chan Status, 10)
	p := &Prober{OnChange: func(s Status) { changes <- s }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx, Probe{Name: "web", Check: f, Interval: 10 * time.Millisecond, Rise: 2, Fall: 2})
	if s := <-changes; !s.Up || s.Name != "web" {
		t.Fatalf("first change %+v", s)
	}
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	if s := <-changes; s.Up || s.LastErr != "down" {
		t.Fatalf("second change %+v", s)
	}
	if st := p.Status(); len(st) != 1 || st[0].Up {
		t.Fatalf("status %+v", st)
	}
}
