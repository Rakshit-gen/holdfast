// Package daemon runs holdfast: BGP sessions to the site's routers, a
// health probe per service, the fleet view, and the guard deciding each
// service's mode from those.
package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Rakshit-gen/holdfast/bgp"
	"github.com/Rakshit-gen/holdfast/config"
	"github.com/Rakshit-gen/holdfast/fleet"
	"github.com/Rakshit-gen/holdfast/guard"
	"github.com/Rakshit-gen/holdfast/health"
)

// Daemon is one site's holdfast.
type Daemon struct {
	cfg      *config.Config
	logf     func(string, ...any)
	sessions map[netip.Addr]*bgp.Session
	prober   *health.Prober
	fleet    *fleet.Node
	fleetTLS *tls.Config // nil serves the fleet listener in plain HTTP
	kick     chan struct{}

	mu      sync.Mutex
	svc     map[string]*serviceState
	stopped bool
	decided time.Time // last run of the guard
}

type serviceState struct {
	cfg     config.Service
	healthy bool
	drained bool // drained through the API
	mode    guard.Mode
	reason  string
	since   time.Time
	held    int  // times the guard kept this site announcing while sick
	holding bool // and whether it is now
	changes int
	// Until leaving, a withdrawn service still announces its routes as
	// they were in mode was, marked for graceful shutdown.
	leaving time.Time
	was     guard.Mode
}

// New sets up a daemon from a checked config.
func New(cfg *config.Config, logf func(string, ...any)) (*Daemon, error) {
	d := &Daemon{
		cfg: cfg, logf: logf, sessions: map[netip.Addr]*bgp.Session{},
		kick: make(chan struct{}, 1), svc: map[string]*serviceState{},
	}
	for _, n := range cfg.Neighbors {
		sc := cfg.Session(n)
		sc.Logf = logf
		d.sessions[n.Address] = bgp.NewSession(sc)
	}
	d.prober = &health.Prober{OnChange: func(s health.Status) {
		if s.Up {
			logf("health %s: up", s.Name)
		} else if s.Held {
			logf("health %s: passing, but held down for flapping until %s", s.Name, s.HeldUntil.Format(time.TimeOnly))
		} else {
			logf("health %s: down: %s", s.Name, s.LastErr)
		}
		d.poke()
	}}
	f := cfg.Fleet
	d.fleet = &fleet.Node{Site: cfg.Site, Interval: time.Duration(f.Interval), Logf: logf}
	if f.TokenFile != "" {
		b, err := os.ReadFile(f.TokenFile)
		if err != nil {
			return nil, err
		}
		d.fleet.Token = strings.TrimSpace(string(b))
		if d.fleet.Token == "" {
			return nil, errors.New(f.TokenFile + " is empty")
		}
	}
	for _, p := range f.Peers {
		d.fleet.Peers = append(d.fleet.Peers, fleet.Peer{Site: p.Site, URL: p.URL})
	}
	if f.CAFile != "" {
		b, err := os.ReadFile(f.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New(f.CAFile + " has no PEM certificates")
		}
		timeout := time.Duration(f.Interval)
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		d.fleet.Client = &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	}
	if f.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(f.TLSCert, f.TLSKey)
		if err != nil {
			return nil, err
		}
		d.fleetTLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	}
	now := time.Now()
	for _, s := range cfg.Services {
		// Every service starts withdrawn: nothing is announced until its
		// checks have passed.
		d.svc[s.Name] = &serviceState{cfg: s, mode: guard.Withdraw, reason: "starting", since: now}
	}
	return d, nil
}

func (d *Daemon) poke() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Run runs everything until ctx ends, then withdraws this site's routes
// and closes the sessions.
func (d *Daemon) Run(ctx context.Context) error {
	var servers []*http.Server
	listen := func(addr string, h http.Handler, tc *tls.Config) error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		if tc != nil {
			ln = tls.NewListener(ln, tc)
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go srv.Serve(ln)
		return nil
	}
	if d.cfg.API != "" {
		if err := listen(d.cfg.API, d.API(), nil); err != nil {
			return err
		}
	}
	if d.cfg.Fleet.Listen != "" {
		mux := http.NewServeMux()
		mux.Handle("/v1/reports", d.fleet)
		if err := listen(d.cfg.Fleet.Listen, mux, d.fleetTLS); err != nil {
			return err
		}
	}
	var bgpLn net.Listener
	if d.cfg.Listen != "" {
		var err error
		if bgpLn, err = net.Listen("tcp", d.cfg.Listen); err != nil {
			return err
		}
		go d.accept(bgpLn)
	}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
		if bgpLn != nil {
			bgpLn.Close()
		}
	}()

	sctx, stopSessions := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, s := range d.sessions {
		wg.Add(1)
		go func() { defer wg.Done(); s.Run(sctx) }()
	}
	for _, s := range d.cfg.Services {
		wg.Add(1)
		go func() { defer wg.Done(); d.prober.Run(ctx, d.cfg.Probe(s)) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); d.fleet.Run(ctx) }()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		d.decide(time.Now())
		select {
		case <-ctx.Done():
			d.shutdown()
			stopSessions()
			wg.Wait()
			return nil
		case <-tick.C:
		case <-d.kick:
		}
	}
}

// accept hands each incoming BGP connection to the passive session for
// its source address, and closes the rest.
func (d *Daemon) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		ap, _ := netip.ParseAddrPort(c.RemoteAddr().String())
		s := d.sessions[ap.Addr().Unmap().WithZone("")]
		if s == nil {
			d.logf("bgp: closing connection from unknown %v", ap.Addr())
			c.Close()
			continue
		}
		go s.Accept(c)
	}
}

// decide runs the guard for every service and pushes the result to the
// sessions.
func (d *Daemon) decide(now time.Time) {
	up := map[string]bool{}
	for _, st := range d.prober.Status() {
		up[st.Name] = st.Up
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.decided = now
	var routes []bgp.Route
	for name, s := range d.svc {
		s.healthy = up[name] && !s.drained
		dec := guard.Decide(guard.Input{
			Service: name, Site: d.cfg.Site, Healthy: s.healthy, Mode: s.mode, Since: s.since,
			Fleet: d.fleet.View(name), Sites: d.fleet.Sites(), Now: now,
		}, d.cfg.Policy(s.cfg))
		if dec.Mode != s.mode {
			d.logf("service %s: %s -> %s (%s)", name, s.mode, dec.Mode, dec.Reason)
			if gs := time.Duration(d.cfg.GracefulShutdown); dec.Mode == guard.Withdraw && s.mode != guard.Withdraw && gs > 0 {
				s.leaving, s.was = now.Add(gs), s.mode
			}
			s.mode, s.since = dec.Mode, now
			s.changes++
		}
		if dec.Held && !s.holding {
			s.held++
		}
		s.holding = dec.Held
		s.reason = dec.Reason
		d.fleet.Set(name, s.healthy, s.mode)
		if s.mode == guard.Withdraw && now.Before(s.leaving) {
			s.reason += "; graceful shutdown until " + s.leaving.Format(time.TimeOnly)
			routes = append(routes, gracefully(d.cfg.Routes(s.cfg, s.was))...)
		} else {
			routes = append(routes, d.cfg.Routes(s.cfg, s.mode)...)
		}
	}
	for _, sess := range d.sessions {
		sess.Advertise(routes)
	}
}

// gracefully marks routes with the GRACEFUL_SHUTDOWN community.
func gracefully(routes []bgp.Route) []bgp.Route {
	for i := range routes {
		a := &routes[i].Attrs
		a.Communities = append(slices.Clip(a.Communities), bgp.CommunityGracefulShutdown)
	}
	return routes
}

// shutdown withdraws everything and gives the sessions a moment to send
// the withdrawals before they close. A clean stop shouldn't leave the
// routers to find out from the hold timer. With graceful_shutdown set, it
// first marks every announced route and waits that long.
func (d *Daemon) shutdown() {
	d.mu.Lock()
	d.stopped = true
	var routes []bgp.Route
	for _, s := range d.svc {
		if s.mode != guard.Withdraw {
			routes = append(routes, gracefully(d.cfg.Routes(s.cfg, s.mode))...)
		}
	}
	d.mu.Unlock()
	if gs := time.Duration(d.cfg.GracefulShutdown); gs > 0 && len(routes) > 0 {
		d.logf("graceful shutdown: keeping routes for %v with the graceful-shutdown community", gs)
		for _, s := range d.sessions {
			s.Advertise(routes)
		}
		time.Sleep(gs)
	}
	for _, s := range d.sessions {
		s.Advertise(nil)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		for _, s := range d.sessions {
			st := s.Status()
			busy = busy || (st.State == bgp.Established && st.Sent > 0)
		}
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Drain marks a service, or every service if name is empty, as unhealthy
// at this site. The guard still decides whether it may withdraw.
func (d *Daemon) Drain(name string, on bool) error {
	d.mu.Lock()
	found := false
	for n, s := range d.svc {
		if name == "" || n == name {
			s.drained, found = on, true
		}
	}
	d.mu.Unlock()
	if !found {
		return errors.New("no service " + name)
	}
	d.poke()
	return nil
}
