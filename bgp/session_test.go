package bgp

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// pair starts an active session a toward a passive session b over
// loopback TCP.
func pair(t *testing.T, ac, bc Config) (a, b *Session) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ac.Peer = netip.MustParseAddrPort(ln.Addr().String())
	bc.Passive = true
	if ac.Retry == 0 {
		ac.Retry = 50 * time.Millisecond
	}
	a, b = NewSession(ac), NewSession(bc)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.Accept(c)
		}
	}()
	return a, b
}

func start(t *testing.T, ss ...*Session) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, len(ss))
	for _, s := range ss {
		go func() { s.Run(ctx); done <- struct{}{} }()
	}
	t.Cleanup(func() {
		cancel()
		for range ss {
			<-done
		}
	})
	return cancel
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cfg(as uint32, id string) Config {
	return Config{
		LocalAS: as, RouterID: netip.MustParseAddr(id),
		NextHop: netip.MustParseAddr(id), NextHop6: netip.MustParseAddr("2001:db8::" + strings.Split(id, ".")[3]),
		HoldTime: 3 * time.Second,
	}
}

func TestSessionAdvertisesAndFollowsChanges(t *testing.T) {
	ac, bc := cfg(64512, "10.0.0.1"), cfg(64513, "10.0.0.2")
	ac.PeerAS, bc.PeerAS = 64513, 64512
	a, b := pair(t, ac, bc)
	a.Advertise([]Route{
		{Prefix: netip.MustParsePrefix("192.0.2.0/24")},
		{Prefix: netip.MustParsePrefix("2001:db8:1::/48")},
	})
	start(t, a, b)
	eventually(t, "both routes at b", func() bool { return len(b.Received()) == 2 })

	got := b.Received()
	if got[0].Prefix.String() != "192.0.2.0/24" || got[0].NextHop.String() != "10.0.0.1" || got[0].Attrs.PathString() != "64512" {
		t.Fatalf("got %+v", got[0])
	}
	if got[1].NextHop.String() != "2001:db8::1" {
		t.Fatalf("v6 next hop %v", got[1].NextHop)
	}
	if st := a.Status(); st.State != Established || st.Sent != 2 || st.Hold != 3*time.Second {
		t.Fatalf("a status %+v", st)
	}

	// Prepending changes the path; dropping the v6 route withdraws it.
	r := Route{Prefix: netip.MustParsePrefix("192.0.2.0/24")}
	r.Attrs.Prepend(64512, 2)
	a.Advertise([]Route{r})
	eventually(t, "prepend and withdraw at b", func() bool {
		got := b.Received()
		return len(got) == 1 && got[0].Attrs.PathLen() == 3
	})
}

func TestSessionStopTellsThePeer(t *testing.T) {
	ac, bc := cfg(64512, "10.0.0.1"), cfg(64513, "10.0.0.2")
	ac.PeerAS, bc.PeerAS = 64513, 64512
	a, b := pair(t, ac, bc)
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	start(t, b)
	eventually(t, "established", func() bool { return b.Status().State == Established })
	cancel()
	eventually(t, "b to see the shutdown", func() bool {
		return strings.Contains(b.Status().LastError, "holdfast is stopping")
	})
	if b.Status().Flaps != 1 {
		t.Fatalf("flaps %d", b.Status().Flaps)
	}
}

func TestSessionRejectsWrongAS(t *testing.T) {
	ac, bc := cfg(64512, "10.0.0.1"), cfg(64513, "10.0.0.2")
	ac.PeerAS, bc.PeerAS = 64513, 65000 // b expects someone else
	a, b := pair(t, ac, bc)
	start(t, a, b)
	eventually(t, "a to hear bad peer AS", func() bool {
		return strings.Contains(a.Status().LastError, "OPEN message error, subcode 2")
	})
	if a.Status().State == Established {
		t.Fatal("established with the wrong AS")
	}
}

func TestSessionHoldTimerExpires(t *testing.T) {
	// A peer that completes the handshake and then goes silent.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		Read(c)
		b, _ := Marshal(&Open{AS: 64513, HoldTime: 3, ID: netip.MustParseAddr("10.0.0.2"), FourOctet: true})
		c.Write(b)
		ka, _ := Marshal(Keepalive{})
		c.Write(ka)
		time.Sleep(10 * time.Second)
	}()
	ac := cfg(64512, "10.0.0.1")
	ac.PeerAS, ac.Peer, ac.Retry = 64513, netip.MustParseAddrPort(ln.Addr().String()), time.Hour
	a := NewSession(ac)
	start(t, a)
	eventually(t, "established", func() bool { return a.Status().State == Established })
	time.Sleep(3500 * time.Millisecond)
	if st := a.Status(); st.State == Established || !strings.Contains(st.LastError, "hold timer expired") {
		t.Fatalf("status %+v", st)
	}
}

func TestSessionMaxPrefixes(t *testing.T) {
	ac, bc := cfg(64512, "10.0.0.1"), cfg(64513, "10.0.0.2")
	ac.PeerAS, bc.PeerAS = 64513, 64512
	bc.MaxPrefixes = 2
	a, b := pair(t, ac, bc)
	var rs []Route
	for i := range 3 {
		rs = append(rs, Route{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, byte(i), 0}), 24)})
	}
	a.Advertise(rs)
	start(t, a, b)
	eventually(t, "a cut off for too many prefixes", func() bool {
		return strings.Contains(a.Status().LastError, "cease, subcode 1")
	})
}

func TestSessionIBGPKeepsPathAndSetsLocalPref(t *testing.T) {
	ac, bc := cfg(64512, "10.0.0.1"), cfg(64512, "10.0.0.2")
	ac.PeerAS, bc.PeerAS = 64512, 64512
	a, b := pair(t, ac, bc)
	a.Advertise([]Route{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}})
	start(t, a, b)
	eventually(t, "route at b", func() bool { return len(b.Received()) == 1 })
	got := b.Received()[0].Attrs
	if got.PathLen() != 0 || !got.HasLocalPref || got.LocalPref != 100 {
		t.Fatalf("got %+v", got)
	}
}

func TestSessionSkipsFamilyThePeerLacks(t *testing.T) {
	ac, bc := cfg(64512, "10.0.0.1"), cfg(64513, "10.0.0.2")
	ac.PeerAS, bc.PeerAS = 64513, 64512
	bc.Families = []Family{IPv4Unicast}
	a, b := pair(t, ac, bc)
	a.Advertise([]Route{
		{Prefix: netip.MustParsePrefix("192.0.2.0/24")},
		{Prefix: netip.MustParsePrefix("2001:db8:1::/48")},
	})
	start(t, a, b)
	eventually(t, "established", func() bool { return a.Status().State == Established })
	eventually(t, "v4 route", func() bool { return len(b.Received()) == 1 })
	if a.Status().Sent != 1 {
		t.Fatalf("sent %d, want only the IPv4 route", a.Status().Sent)
	}
}

func TestPlanPacksAndGroups(t *testing.T) {
	var rs []Route
	for i := range 600 {
		rs = append(rs, Route{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 0}), 24), NextHop: netip.MustParseAddr("192.0.2.1")})
	}
	odd := Route{Prefix: netip.MustParsePrefix("198.51.100.0/24"), NextHop: netip.MustParseAddr("192.0.2.1")}
	odd.Attrs.Communities = []uint32{CommunityNoExport}
	rs = append(rs, odd)
	us := Plan(nil, rs, 64512, 64513)
	total := 0
	for _, u := range us {
		b, err := Marshal(u)
		if err != nil || len(b) > MaxLen {
			t.Fatalf("update of %d bytes: %v", len(b), err)
		}
		total += len(u.Announce)
		if u.Attrs.PathString() != "64512" {
			t.Fatalf("path %q", u.Attrs.PathString())
		}
	}
	if total != 601 || len(us) < 3 {
		t.Fatalf("%d prefixes in %d updates", total, len(us))
	}
	// The input routes must not have been prepended in place.
	if rs[0].Attrs.PathLen() != 0 {
		t.Fatal("Plan changed its input")
	}
}

func TestSessionAnswersRouteRefresh(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	pf := netip.MustParsePrefix("198.51.100.0/24")
	got := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		Read(c)
		b, _ := Marshal(&Open{AS: 64513, HoldTime: 3, ID: netip.MustParseAddr("10.0.0.2"), FourOctet: true, RouteRefresh: true, Families: []Family{IPv4Unicast}})
		c.Write(b)
		ka, _ := Marshal(Keepalive{})
		c.Write(ka)
		// Wait for the route and the End-of-RIB, then ask again.
		announced := func() bool {
			for {
				m, err := Read(c)
				if err != nil {
					got <- err
					return false
				}
				if u, ok := m.(*Update); ok && slices.Contains(u.Announce, pf) {
					return true
				}
			}
		}
		if !announced() {
			return
		}
		rr, _ := Marshal(&RouteRefresh{IPv4Unicast})
		c.Write(rr)
		if announced() {
			got <- nil
		}
	}()
	ac := cfg(64512, "10.0.0.1")
	ac.PeerAS, ac.Peer, ac.Retry = 64513, netip.MustParseAddrPort(ln.Addr().String()), time.Hour
	a := NewSession(ac)
	a.Advertise([]Route{{Prefix: pf}})
	start(t, a)
	if err := <-got; err != nil {
		t.Fatal(err)
	}
}

func TestPassiveSessionTakesTheNextConnection(t *testing.T) {
	ac, bc := cfg(64512, "10.0.0.1"), cfg(64513, "10.0.0.2")
	ac.PeerAS, bc.PeerAS = 64513, 64512
	a, b := pair(t, ac, bc)
	pf := netip.MustParsePrefix("198.51.100.0/24")
	a.Advertise([]Route{{Prefix: pf}})
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	start(t, b)
	eventually(t, "established", func() bool { return len(b.Received()) == 1 })
	cancel()
	eventually(t, "b waiting again", func() bool { return b.Status().State == Active })
	if len(b.Received()) != 0 {
		t.Fatal("routes kept after the session dropped")
	}
	// The peer comes back as a new process.
	a2 := NewSession(a.cfg)
	a2.Advertise([]Route{{Prefix: pf}})
	start(t, a2)
	eventually(t, "re-established with the route", func() bool { return len(b.Received()) == 1 })
}
