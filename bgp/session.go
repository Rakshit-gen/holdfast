package bgp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// State is a session's place in the RFC 4271 state machine.
type State int

// Session states.
const (
	Idle State = iota
	Connect
	Active
	OpenSent
	OpenConfirm
	Established
)

var stateNames = []string{"idle", "connect", "active", "opensent", "openconfirm", "established"}

func (s State) String() string { return stateNames[s] }

// Config is one peering.
type Config struct {
	LocalAS  uint32
	PeerAS   uint32
	RouterID netip.Addr
	// Peer is where to connect. With Passive, the session waits for the
	// peer to connect instead, through Accept.
	Peer    netip.AddrPort
	Passive bool
	// LocalAddr, if set, is the source address to dial from.
	LocalAddr netip.Addr
	// HoldTime is what we offer; the session uses the smaller of ours and
	// the peer's. 0 means 90 seconds. Negative means no hold timer.
	HoldTime time.Duration
	// Families to offer. Empty means IPv4 and IPv6 unicast.
	Families []Family
	// NextHop and NextHop6 are used for routes that don't set their own.
	NextHop, NextHop6 netip.Addr
	// MaxPrefixes caps how many routes we keep from the peer. An announcer
	// doesn't need the peer's routes, and a router that sends a full table
	// by mistake shouldn't take 1 GB with it. 0 means 10000.
	MaxPrefixes int
	// GracefulRestart offers RFC 4724 with this restart time, so a peer
	// keeps our routes while holdfast restarts. 0 leaves it off: if
	// holdfast dies, its routes go with it.
	GracefulRestart time.Duration
	// Retry is the first wait between connection attempts; it doubles up
	// to 2 minutes. 0 means 5 seconds.
	Retry time.Duration
	Dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	Logf  func(format string, args ...any)
}

func (c *Config) defaults() {
	if c.HoldTime == 0 {
		c.HoldTime = 90 * time.Second
	}
	if len(c.Families) == 0 {
		c.Families = []Family{IPv4Unicast, IPv6Unicast}
	}
	if c.MaxPrefixes == 0 {
		c.MaxPrefixes = 10000
	}
	if c.Retry == 0 {
		c.Retry = 5 * time.Second
	}
	if c.Dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second}
		if c.LocalAddr.IsValid() {
			d.LocalAddr = &net.TCPAddr{IP: c.LocalAddr.AsSlice()}
		}
		c.Dial = d.DialContext
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

// Route is a prefix to announce with its attributes. A zero NextHop means
// the session's own.
type Route struct {
	Prefix  netip.Prefix
	Attrs   Attrs
	NextHop netip.Addr
}

// Status is a snapshot of a session.
type Status struct {
	State     State
	Since     time.Time // when it entered State
	PeerOpen  *Open     // the peer's OPEN, once received
	Hold      time.Duration
	Sent      int // prefixes the peer has from us
	Received  int
	Flaps     int // times it left Established
	LastError string
}

// Session keeps one BGP peering up and the peer's view of our routes in
// line with what Advertise last asked for.
type Session struct {
	cfg Config

	mu     sync.Mutex
	want   map[netip.Prefix]Route
	status Status
	recv   map[netip.Prefix]Route

	wake     chan struct{}
	incoming chan net.Conn
}

// NewSession makes a session; Run starts it.
func NewSession(cfg Config) *Session {
	cfg.defaults()
	return &Session{
		cfg:      cfg,
		want:     map[netip.Prefix]Route{},
		recv:     map[netip.Prefix]Route{},
		wake:     make(chan struct{}, 1),
		incoming: make(chan net.Conn),
		status:   Status{State: Idle, Since: time.Now()},
	}
}

// Advertise replaces the set of routes the peer should have. The session
// sends only the difference.
func (s *Session) Advertise(routes []Route) {
	m := make(map[netip.Prefix]Route, len(routes))
	for _, r := range routes {
		r.Prefix = r.Prefix.Masked()
		m[r.Prefix] = r
	}
	s.mu.Lock()
	s.want = m
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Status returns a snapshot.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Received returns the routes the peer sent, sorted by prefix.
func (s *Session) Received() []Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Route, 0, len(s.recv))
	for _, r := range s.recv {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Route) int { return a.Prefix.Addr().Compare(b.Prefix.Addr()) })
	return out
}

// Accept hands the session a connection the peer opened. It is closed if
// the session isn't waiting for one.
func (s *Session) Accept(c net.Conn) {
	select {
	case s.incoming <- c:
	case <-time.After(time.Second):
		c.Close()
	}
}

func (s *Session) setState(st State, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.State == Established && st != Established {
		s.status.Flaps++
		s.status.Sent = 0
		s.recv = map[netip.Prefix]Route{}
		s.status.Received = 0
	}
	if st != s.status.State {
		s.status.Since = time.Now()
	}
	s.status.State = st
	if err != nil {
		s.status.LastError = err.Error()
	}
}

// Run keeps the session up until ctx ends. Then it tells the peer it is
// shutting down and returns.
func (s *Session) Run(ctx context.Context) error {
	wait := s.cfg.Retry
	for {
		conn, err := s.connect(ctx)
		if err == nil {
			up := time.Now()
			err = s.run(ctx, conn)
			conn.Close()
			if time.Since(up) > 2*time.Minute {
				wait = s.cfg.Retry
			}
		}
		if ctx.Err() != nil {
			s.setState(Idle, nil)
			return nil
		}
		s.setState(Idle, err)
		if s.cfg.Passive {
			s.cfg.Logf("bgp %s: %v; waiting for the peer to reconnect", s.name(), err)
			continue
		}
		s.cfg.Logf("bgp %s: %v; retrying in %v", s.name(), err, wait)
		var n *Notification
		if errors.As(err, &n) && n.Code == ErrCease && n.Subcode == CeaseMaxPrefixes {
			wait = max(wait, time.Minute)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(2*wait, 2*time.Minute)
	}
}

// name is how logs refer to the peer.
func (s *Session) name() string {
	if s.cfg.Peer.IsValid() {
		return s.cfg.Peer.String()
	}
	return fmt.Sprintf("AS%d", s.cfg.PeerAS)
}

func (s *Session) connect(ctx context.Context) (net.Conn, error) {
	if s.cfg.Passive {
		s.setState(Active, nil)
		select {
		case c := <-s.incoming:
			return c, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.setState(Connect, nil)
	return s.cfg.Dial(ctx, "tcp", s.cfg.Peer.String())
}

func (s *Session) open() *Open {
	o := &Open{
		AS: s.cfg.LocalAS, ID: s.cfg.RouterID, Families: s.cfg.Families,
		FourOctet: true, RouteRefresh: true,
	}
	if s.cfg.HoldTime > 0 {
		o.HoldTime = uint16(s.cfg.HoldTime / time.Second)
	}
	if s.cfg.GracefulRestart > 0 {
		o.GracefulRestart = true
		o.Restart = uint16(min(s.cfg.GracefulRestart/time.Second, 4095))
	}
	return o
}

// conn wraps the socket with the negotiated codec and write deadlines.
type conn struct {
	net.Conn
	codec Codec
	mu    sync.Mutex // the shutdown on cancel can race the main loop
}

func (c *conn) send(m Message) error {
	b, err := c.codec.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err = c.Write(b)
	return err
}

// deadline sets the read deadline d from now, or none for 0. A cancel
// that lands before this would have its "unblock now" deadline
// overwritten, so check for it after.
func (c *conn) deadline(ctx context.Context, d time.Duration) error {
	var t time.Time
	if d > 0 {
		t = time.Now().Add(d)
	}
	c.SetReadDeadline(t)
	return ctx.Err()
}

// checkOpen validates the peer's OPEN against the config.
func (s *Session) checkOpen(o *Open) error {
	if o.AS != s.cfg.PeerAS {
		return &Notification{Code: ErrOpen, Subcode: ErrOpenPeerAS}
	}
	if o.ID == s.cfg.RouterID && s.cfg.LocalAS == s.cfg.PeerAS {
		return &Notification{Code: ErrOpen, Subcode: ErrOpenID}
	}
	shared := false
	for _, f := range s.cfg.Families {
		shared = shared || o.Supports(f)
	}
	if !shared {
		return &Notification{Code: ErrOpen, Subcode: ErrOpenCapability}
	}
	return nil
}

func (s *Session) run(ctx context.Context, nc net.Conn) error {
	c := &conn{Conn: nc}
	// When ctx ends, unblock any read, then tell the peer why we're going.
	stop := context.AfterFunc(ctx, func() { c.SetReadDeadline(time.Now()) })
	defer stop()
	defer func() {
		if ctx.Err() != nil {
			c.send(Shutdown("holdfast is stopping"))
		}
	}()

	if err := c.send(s.open()); err != nil {
		return err
	}
	s.setState(OpenSent, nil)
	// RFC 4271 suggests 4 minutes for the OPEN to arrive.
	if err := c.deadline(ctx, 4*time.Minute); err != nil {
		return err
	}
	m, err := c.codec.Read(c)
	if err != nil {
		return s.fail(c, err)
	}
	peer, ok := m.(*Open)
	if !ok {
		return s.fail(c, notOpen(m))
	}
	if err := s.checkOpen(peer); err != nil {
		return s.fail(c, err)
	}
	c.codec.TwoOctetAS = !peer.FourOctet
	// The smaller hold time wins, and either side offering 0 turns it off.
	hold := time.Duration(peer.HoldTime) * time.Second
	if ours := time.Duration(s.open().HoldTime) * time.Second; ours < hold {
		hold = ours
	}
	if err := c.send(Keepalive{}); err != nil {
		return err
	}
	s.mu.Lock()
	s.status.PeerOpen, s.status.Hold = peer, hold
	s.mu.Unlock()
	s.setState(OpenConfirm, nil)

	msgs := make(chan Message)
	errc := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			err := c.deadline(ctx, hold)
			var m Message
			if err == nil {
				m, err = c.codec.Read(c)
			}
			if err != nil {
				var ne net.Error
				if ctx.Err() != nil {
					err = ctx.Err()
				} else if errors.As(err, &ne) && ne.Timeout() {
					err = &Notification{Code: ErrHoldTimer}
				}
				errc <- err
				return
			}
			select {
			case msgs <- m:
			case <-done:
				return
			}
		}
	}()

	var tick <-chan time.Time
	if hold > 0 {
		t := time.NewTicker(hold / 3)
		defer t.Stop()
		tick = t.C
	}
	sent := map[netip.Prefix]Route{}
	established := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return s.fail(c, err)
		case <-tick:
			if err := c.send(Keepalive{}); err != nil {
				return err
			}
		case <-s.wake:
			if established {
				if err := s.sync(c, peer, sent); err != nil {
					return err
				}
			}
		case m := <-msgs:
			switch m := m.(type) {
			case Keepalive:
				if !established {
					established = true
					s.setState(Established, nil)
					s.cfg.Logf("bgp %s: established, AS%d, hold %v", s.name(), peer.AS, hold)
					if err := s.sync(c, peer, sent); err != nil {
						return err
					}
					for _, f := range s.cfg.Families {
						if peer.Supports(f) {
							if err := c.send(EndOfRIB(f)); err != nil {
								return err
							}
						}
					}
				}
			case *Update:
				if !established {
					return s.fail(c, &Notification{Code: ErrFSM})
				}
				if err := s.receive(m); err != nil {
					return s.fail(c, err)
				}
			case *RouteRefresh:
				// The peer asks for everything in a family again, say after
				// changing its import policy. Families we never offered
				// are ignored, as RFC 2918 says.
				if established && peer.Supports(m.Family) && slices.Contains(s.cfg.Families, m.Family) {
					if err := s.resend(c, peer, sent, m.Family); err != nil {
						return err
					}
				}
			case *Notification:
				return fmt.Errorf("peer sent %w", m)
			case *Open:
				return s.fail(c, &Notification{Code: ErrFSM})
			}
		}
	}
}

func notOpen(m Message) error {
	if n, ok := m.(*Notification); ok {
		return fmt.Errorf("peer sent %w", n)
	}
	return &Notification{Code: ErrFSM}
}

// fail sends a NOTIFICATION for protocol errors and returns the error.
func (s *Session) fail(c *conn, err error) error {
	var n *Notification
	if errors.As(err, &n) && !strings.HasPrefix(err.Error(), "peer sent") {
		c.send(n)
	}
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("peer closed the connection")
	}
	return err
}

func (s *Session) receive(u *Update) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range u.Withdraw {
		delete(s.recv, p)
	}
	for _, p := range u.Announce {
		nh := u.NextHop
		if p.Addr().Is6() {
			nh = u.NextHop6
		}
		s.recv[p] = Route{Prefix: p, Attrs: u.Attrs, NextHop: nh}
	}
	s.status.Received = len(s.recv)
	if len(s.recv) > s.cfg.MaxPrefixes {
		return &Notification{Code: ErrCease, Subcode: CeaseMaxPrefixes}
	}
	return nil
}

// sync sends what changed between sent and the wanted routes, and updates
// sent to match.
func (s *Session) sync(c *conn, peer *Open, sent map[netip.Prefix]Route) error {
	s.mu.Lock()
	want := make(map[netip.Prefix]Route, len(s.want))
	for p, r := range s.want {
		f := IPv4Unicast
		if p.Addr().Is6() {
			f = IPv6Unicast
		}
		if peer.Supports(f) && slices.Contains(s.cfg.Families, f) {
			if !r.NextHop.IsValid() {
				r.NextHop = s.cfg.NextHop
				if p.Addr().Is6() {
					r.NextHop = s.cfg.NextHop6
				}
			}
			if r.NextHop.Is4() != p.Addr().Is4() || !r.NextHop.IsValid() {
				s.cfg.Logf("bgp %s: not announcing %v, no %s next hop", s.name(), p, f)
				continue
			}
			want[p] = r
		}
	}
	s.mu.Unlock()

	var gone []netip.Prefix
	for p := range sent {
		if _, ok := want[p]; !ok {
			gone = append(gone, p)
		}
	}
	for _, u := range Plan(gone, changed(sent, want), s.cfg.LocalAS, peer.AS) {
		if err := c.send(u); err != nil {
			return err
		}
	}
	clear(sent)
	for p, r := range want {
		sent[p] = r
	}
	s.mu.Lock()
	s.status.Sent = len(sent)
	s.mu.Unlock()
	return nil
}

// resend sends every route in a family again.
func (s *Session) resend(c *conn, peer *Open, sent map[netip.Prefix]Route, f Family) error {
	var routes []Route
	for p, r := range sent {
		if p.Addr().Is6() == (f == IPv6Unicast) {
			routes = append(routes, r)
		}
	}
	s.cfg.Logf("bgp %s: route refresh for %v, sending %d routes", s.name(), f, len(routes))
	for _, u := range Plan(nil, routes, s.cfg.LocalAS, peer.AS) {
		if err := c.send(u); err != nil {
			return err
		}
	}
	return nil
}

func changed(sent, want map[netip.Prefix]Route) []Route {
	var out []Route
	for p, r := range want {
		if old, ok := sent[p]; !ok || !sameRoute(old, r) {
			out = append(out, r)
		}
	}
	return out
}

func sameRoute(a, b Route) bool {
	return a.NextHop == b.NextHop && attrKey(&a.Attrs) == attrKey(&b.Attrs)
}
