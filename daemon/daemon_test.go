package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rakshit-gen/holdfast/bgp"
	"github.com/Rakshit-gen/holdfast/config"
)

// router is a passive BGP speaker standing in for a site's edge router.
type router struct {
	s    *bgp.Session
	port uint16
}

func newRouter(t *testing.T, ctx context.Context) *router {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := bgp.NewSession(bgp.Config{LocalAS: 64513, PeerAS: 64512, RouterID: netip.MustParseAddr("10.9.9.9"), Passive: true, HoldTime: 3 * time.Second})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.Accept(c)
		}
	}()
	go s.Run(ctx)
	return &router{s: s, port: uint16(ln.Addr().(*net.TCPAddr).Port)}
}

// paths returns the AS path of each received route, by prefix.
func (r *router) paths() map[string]string {
	out := map[string]string{}
	for _, rt := range r.s.Received() {
		out[rt.Prefix.String()] = rt.Attrs.PathString()
	}
	return out
}

type site struct {
	d       *Daemon
	r       *router
	healthy *atomic.Bool
	api     string
	stop    context.CancelFunc
	done    chan struct{}
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// fleetOf starts n sites announcing one prefix, each with its own router
// and a health endpoint the test can turn off.
func fleetOf(t *testing.T, n int, edit func(*config.Config)) []*site {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	names := make([]string, n)
	fleetAddrs := make([]string, n)
	for i := range n {
		names[i] = fmt.Sprintf("s%d", i)
		fleetAddrs[i] = freePort(t)
	}
	var sites []*site
	for i := range n {
		healthy := &atomic.Bool{}
		healthy.Store(true)
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !healthy.Load() {
				w.WriteHeader(503)
			}
		}))
		t.Cleanup(hs.Close)
		r := newRouter(t, ctx)
		cfg := &config.Config{
			Site: names[i], ASN: 64512, RouterID: netip.MustParseAddr(fmt.Sprintf("10.0.0.%d", i+1)),
			NextHop: netip.MustParseAddr(fmt.Sprintf("10.0.0.%d", i+1)), API: freePort(t),
			Neighbors: []config.Neighbor{{Address: netip.MustParseAddr("127.0.0.1"), Port: r.port, ASN: 64513, HoldTime: config.Duration(3 * time.Second)}},
			Services: []config.Service{{
				Name: "dns", Prefixes: []string{"198.51.100.53/32"},
				Checks:   []config.Check{{Type: "http", URL: hs.URL}},
				Interval: config.Duration(20 * time.Millisecond), Rise: 1, Fall: 1,
			}},
			Fleet: config.Fleet{Listen: fleetAddrs[i], Interval: config.Duration(20 * time.Millisecond), Stale: config.Duration(2 * time.Second), MinUp: 2},
		}
		for j := range n {
			if j != i {
				cfg.Fleet.Peers = append(cfg.Fleet.Peers, config.Peer{Site: names[j], URL: "http://" + fleetAddrs[j]})
			}
		}
		if edit != nil {
			edit(cfg)
		}
		if err := cfg.Check(); err != nil {
			t.Fatal(err)
		}
		d, err := New(cfg, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		sctx, stop := context.WithCancel(ctx)
		s := &site{d: d, r: r, healthy: healthy, api: "http://" + cfg.API, stop: stop, done: make(chan struct{})}
		go func() { d.Run(sctx); close(s.done) }()
		t.Cleanup(func() { stop(); <-s.done })
		sites = append(sites, s)
	}
	return sites
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(8 * time.Second); !f(); {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// count sorts the routers by what they hear: announced plainly,
// announced with a prepended path, or nothing.
func count(sites []*site) (plain, degraded, gone int) {
	for _, s := range sites {
		switch p, ok := s.r.paths()["198.51.100.53/32"]; {
		case !ok:
			gone++
		case p == "64512":
			plain++
		default:
			degraded++
		}
	}
	return
}

func TestAllHealthyAllAnnounce(t *testing.T) {
	sites := fleetOf(t, 3, nil)
	eventually(t, "every router to hear the route", func() bool {
		p, _, _ := count(sites)
		return p == 3
	})
}

func TestOneSickSiteWithdraws(t *testing.T) {
	sites := fleetOf(t, 3, nil)
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 3 })
	sites[1].healthy.Store(false)
	eventually(t, "s1 withdrawn", func() bool {
		_, ok := sites[1].r.paths()["198.51.100.53/32"]
		p, _, _ := count(sites)
		return !ok && p == 2
	})
}

// The case the tool exists for: every site's check fails at once. With
// min_up 2, one site withdraws and two stay, degraded.
func TestEverySiteSickKeepsTheFloor(t *testing.T) {
	sites := fleetOf(t, 3, nil)
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 3 })
	for _, s := range sites {
		s.healthy.Store(false)
	}
	eventually(t, "two degraded, one gone", func() bool {
		p, d, g := count(sites)
		return p == 0 && d == 2 && g == 1
	})
	// And it holds there.
	time.Sleep(500 * time.Millisecond)
	if p, d, g := count(sites); d != 2 || g != 1 || p != 0 {
		t.Fatalf("drifted to %d plain, %d degraded, %d gone", p, d, g)
	}
	for _, s := range sites {
		s.healthy.Store(true)
	}
	eventually(t, "recovered", func() bool { p, _, _ := count(sites); return p == 3 })
}

func TestUnguardedLosesEverything(t *testing.T) {
	sites := fleetOf(t, 3, func(c *config.Config) { c.Fleet.Unguarded = true })
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 3 })
	for _, s := range sites {
		s.healthy.Store(false)
	}
	eventually(t, "all gone", func() bool { _, _, g := count(sites); return g == 3 })
}

func TestCutOffSiteFailsStatic(t *testing.T) {
	sites := fleetOf(t, 3, nil)
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 3 })
	// s0 loses sight of the fleet, then its check fails. It can't tell
	// whether the others are still up, so it degrades instead of leaving.
	sites[0].d.fleet.Peers = nil // stop polling; the old reports go stale
	time.Sleep(2200 * time.Millisecond)
	sites[0].healthy.Store(false)
	eventually(t, "s0 degraded", func() bool {
		p, ok := sites[0].r.paths()["198.51.100.53/32"]
		return ok && p != "64512"
	})
}

func TestDrainThroughTheAPI(t *testing.T) {
	sites := fleetOf(t, 3, nil)
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 3 })
	resp, err := http.Post(sites[2].api+"/v1/drain?service=dns", "", nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("drain: %v %v", resp, err)
	}
	eventually(t, "s2 out", func() bool { _, ok := sites[2].r.paths()["198.51.100.53/32"]; return !ok })
	if resp, _ := http.Post(sites[2].api+"/v1/drain?service=nope", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("draining a missing service: %d", resp.StatusCode)
	}
	// A second drain hits the floor.
	http.Post(sites[1].api+"/v1/drain", "", nil)
	eventually(t, "one plain, one degraded, one gone", func() bool {
		p, d, g := count(sites)
		return p == 1 && d == 1 && g == 1
	})
	http.Post(sites[2].api+"/v1/undrain", "", nil)
	http.Post(sites[1].api+"/v1/undrain", "", nil)
	eventually(t, "back", func() bool { p, _, _ := count(sites); return p == 3 })
}

func TestStatusAndMetrics(t *testing.T) {
	sites := fleetOf(t, 3, nil)
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 3 })
	eventually(t, "every site seen announcing", func() bool {
		n := 0
		for _, x := range sites[0].d.Status().Services[0].Sites {
			if x.Mode == "announce" {
				n++
			}
		}
		return n == 3
	})
	st := sites[0].d.Status()
	if st.Services[0].Mode != "announce" || st.Neighbors[0].State != "established" || st.Neighbors[0].Sent != 1 {
		t.Fatalf("status %+v", st)
	}
	resp, err := http.Get(sites[0].api + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`holdfast_service_mode{service="dns",mode="announce"} 0`,
		`holdfast_fleet_sites_announcing{service="dns"} 3`,
		`holdfast_guard_holding{service="dns"} 0`,
		`holdfast_service_changes_total{service="dns"} 1`,
		`holdfast_bgp_established{neighbor="127.0.0.1"} 1`,
		`holdfast_fleet_peer_reachable{site="s1"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestStopWithdrawsFirst(t *testing.T) {
	sites := fleetOf(t, 2, func(c *config.Config) { c.Fleet.MinUp = 1 })
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 2 })
	sites[0].stop()
	<-sites[0].done
	// The router saw the withdrawal before the session closed, so the
	// last thing it heard was the shutdown, with no routes left.
	eventually(t, "the router to hear the shutdown", func() bool {
		return strings.Contains(sites[0].r.s.Status().LastError, "holdfast is stopping")
	})
	if f := sites[0].r.s.Status().Flaps; f != 1 {
		t.Fatalf("%d flaps", f)
	}
}

func TestGracefulShutdownBeforeWithdraw(t *testing.T) {
	sites := fleetOf(t, 2, func(c *config.Config) {
		c.Fleet.MinUp = 1
		c.GracefulShutdown = config.Duration(time.Second)
	})
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 2 })
	marked := func() bool {
		for _, r := range sites[0].r.s.Received() {
			if slices.Contains(r.Attrs.Communities, bgp.CommunityGracefulShutdown) {
				return true
			}
		}
		return false
	}
	sites[0].healthy.Store(false)
	eventually(t, "the route marked for graceful shutdown", marked)
	eventually(t, "the route withdrawn", func() bool { _, ok := sites[0].r.paths()["198.51.100.53/32"]; return !ok })
}

// selfSigned writes a certificate for 127.0.0.1 and its key, and returns
// their paths. The certificate is its own CA.
func selfSigned(t *testing.T) (cert, key string) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "holdfast test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cert, key
}

func TestFleetOverTLS(t *testing.T) {
	cert, key := selfSigned(t)
	sites := fleetOf(t, 2, func(c *config.Config) {
		c.Fleet.TLSCert, c.Fleet.TLSKey, c.Fleet.CAFile = cert, key, cert
		for i := range c.Fleet.Peers {
			c.Fleet.Peers[i].URL = strings.Replace(c.Fleet.Peers[i].URL, "http://", "https://", 1)
		}
	})
	eventually(t, "each site to see the other over TLS", func() bool {
		for _, s := range sites {
			if len(s.d.Status().Services[0].Sites) != 2 || len(s.d.Status().Fleet) != 0 {
				return false
			}
		}
		return true
	})
}

func TestDrainFile(t *testing.T) {
	drain := filepath.Join(t.TempDir(), "drain")
	sites := fleetOf(t, 2, func(c *config.Config) {
		c.Fleet.MinUp = 1
		if c.Site == "s0" {
			c.DrainFile = drain
		}
	})
	eventually(t, "announced", func() bool { p, _, _ := count(sites); return p == 2 })
	os.WriteFile(drain, nil, 0o644)
	eventually(t, "s0 to withdraw", func() bool { p, _, g := count(sites); return p == 1 && g == 1 })
	os.Remove(drain)
	eventually(t, "s0 back", func() bool { p, _, _ := count(sites); return p == 2 })
}

func TestUnknownBGPPeerIsClosed(t *testing.T) {
	listen := freePort(t)
	cfg := &config.Config{
		Site: "s0", ASN: 64512, RouterID: netip.MustParseAddr("10.0.0.1"), NextHop: netip.MustParseAddr("10.0.0.1"),
		Listen:    listen,
		Neighbors: []config.Neighbor{{Address: netip.MustParseAddr("10.9.9.1"), ASN: 64513, Passive: true}},
		Services: []config.Service{{
			Name: "dns", Prefixes: []string{"198.51.100.53/32"}, Checks: []config.Check{{Type: "tcp", Addr: listen}},
		}},
	}
	if err := cfg.Check(); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	var mu sync.Mutex
	d, err := New(cfg, func(f string, a ...any) { mu.Lock(); fmt.Fprintf(&logs, f+"\n", a...); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	var c net.Conn
	eventually(t, "the listener", func() bool { c, err = net.Dial("tcp", listen); return err == nil })
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read %v, want the connection closed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "closing connection from unknown 127.0.0.1") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}
