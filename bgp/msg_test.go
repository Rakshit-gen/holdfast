package bgp

import (
	"bytes"
	"encoding/hex"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, c Codec, m Message) Message {
	t.Helper()
	b, err := c.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("read back %x: %v", b, err)
	}
	return got
}

func TestKeepaliveBytes(t *testing.T) {
	b, err := Marshal(Keepalive{})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("ff", 16) + "001304"
	if hex.EncodeToString(b) != want {
		t.Fatalf("got %x, want %s", b, want)
	}
}

func TestReadRejectsBadHeaders(t *testing.T) {
	ka, _ := Marshal(Keepalive{})
	cases := map[string]struct {
		mut  func([]byte) []byte
		sub  uint8
		code uint8
	}{
		"marker":     {func(b []byte) []byte { b[3] = 0; return b }, ErrHeaderSync, ErrHeader},
		"short":      {func(b []byte) []byte { b[17] = 18; return b }, ErrHeaderLength, ErrHeader},
		"type":       {func(b []byte) []byte { b[18] = 9; return b }, ErrHeaderType, ErrHeader},
		"keepalive+": {func(b []byte) []byte { b[17] = 20; return append(b, 0) }, ErrHeaderLength, ErrHeader},
	}
	for name, c := range cases {
		b := c.mut(append([]byte(nil), ka...))
		_, err := Read(bytes.NewReader(b))
		var n *Notification
		if !errors.As(err, &n) || n.Code != c.code || n.Subcode != c.sub {
			t.Errorf("%s: got %v, want code %d subcode %d", name, err, c.code, c.sub)
		}
	}
}

func TestNotificationShutdownMessage(t *testing.T) {
	got := roundTrip(t, Codec{}, Shutdown("draining site for maintenance")).(*Notification)
	if !strings.Contains(got.Error(), `"draining site for maintenance"`) || got.Code != ErrCease {
		t.Fatalf("got %v", got)
	}
}

func TestRouteRefresh(t *testing.T) {
	b, _ := Marshal(&RouteRefresh{IPv6Unicast})
	if want := []byte{0, 23, TypeRouteRefresh, 0, 2, 0, 1}; !bytes.Equal(b[16:], want) {
		t.Fatalf("% x", b[16:])
	}
	if got := roundTrip(t, Codec{}, &RouteRefresh{IPv6Unicast}).(*RouteRefresh); got.Family != IPv6Unicast {
		t.Fatal(got)
	}
	if _, err := Parse(TypeRouteRefresh, []byte{0, 1, 0}); err == nil {
		t.Fatal("short refresh parsed")
	}
}

func TestOpenRoundTrip(t *testing.T) {
	o := &Open{
		AS: 4200000001, HoldTime: 90, ID: netip.MustParseAddr("192.0.2.1"),
		Families: []Family{IPv4Unicast, IPv6Unicast}, FourOctet: true, RouteRefresh: true,
		GracefulRestart: true, Restart: 120,
		Other: []Capability{{Code: 70, Value: []byte{1, 2}}},
	}
	b, err := Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	// The 2-byte AS field carries AS_TRANS when the AS doesn't fit.
	if b[20] != ASTrans>>8 || b[21] != ASTrans&0xff {
		t.Fatalf("my AS field %x, want AS_TRANS", b[20:22])
	}
	got := roundTrip(t, Codec{}, o).(*Open)
	if !reflect.DeepEqual(got, o) {
		t.Fatalf("got %+v\nwant %+v", got, o)
	}
}

func TestOpenWithoutCapabilities(t *testing.T) {
	o := &Open{AS: 64512, HoldTime: 0, ID: netip.MustParseAddr("10.0.0.1")}
	got := roundTrip(t, Codec{}, o).(*Open)
	if got.AS != 64512 || got.FourOctet || !got.Supports(IPv4Unicast) || got.Supports(IPv6Unicast) {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenRejects(t *testing.T) {
	good, _ := Marshal(&Open{AS: 64512, HoldTime: 90, ID: netip.MustParseAddr("10.0.0.1")})
	cases := map[string]struct {
		at  int
		v   byte
		sub uint8
	}{
		"version":   {19, 3, ErrOpenVersion},
		"hold time": {23, 2, ErrOpenHoldTime},
	}
	for name, c := range cases {
		b := append([]byte(nil), good...)
		b[c.at] = c.v
		_, err := Read(bytes.NewReader(b))
		var n *Notification
		if !errors.As(err, &n) || n.Code != ErrOpen || n.Subcode != c.sub {
			t.Errorf("%s: got %v", name, err)
		}
	}
	b := append([]byte(nil), good...)
	copy(b[24:28], []byte{0, 0, 0, 0})
	if _, err := Read(bytes.NewReader(b)); err == nil {
		t.Error("router ID 0.0.0.0 accepted")
	}
}

func pfx(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func TestUpdateRoundTripBothFamilies(t *testing.T) {
	u := &Update{
		Withdraw: pfx("198.51.100.0/24", "2001:db8:ff::/48"),
		Announce: pfx("203.0.113.0/24", "192.0.2.128/25", "2001:db8::/32"),
		Attrs: Attrs{
			Origin: OriginIGP, ASPath: []Segment{{ASNs: []uint32{64512, 4200000001}}},
			MED: 50, HasMED: true, Communities: []uint32{64512<<16 | 100, CommunityNoExport},
			LargeCommunities: []LargeCommunity{{4200000001, 1, 2}},
		},
		NextHop: netip.MustParseAddr("192.0.2.1"), NextHop6: netip.MustParseAddr("2001:db8::1"),
	}
	got := roundTrip(t, Codec{}, u).(*Update)
	if !reflect.DeepEqual(got, u) {
		t.Fatalf("got %+v\nwant %+v", got, u)
	}
	if _, eor := got.IsEndOfRIB(); eor {
		t.Fatal("a real update read as End-of-RIB")
	}
}

func TestEndOfRIB(t *testing.T) {
	for _, f := range []Family{IPv4Unicast, IPv6Unicast} {
		got := roundTrip(t, Codec{}, EndOfRIB(f)).(*Update)
		if g, ok := got.IsEndOfRIB(); !ok || g != f {
			t.Errorf("%v: read back as %v %v", f, g, ok)
		}
	}
}

func TestTwoOctetPeerGetsAS4Path(t *testing.T) {
	u := &Update{
		Announce: pfx("203.0.113.0/24"),
		Attrs:    Attrs{ASPath: []Segment{{ASNs: []uint32{4200000001, 4200000001, 64512}}}},
		NextHop:  netip.MustParseAddr("192.0.2.1"),
	}
	c := Codec{TwoOctetAS: true}
	b, _ := c.Marshal(u)
	if !bytes.Contains(b, []byte{FlagOptional | FlagTransitive, AttrAS4Path}) {
		t.Fatal("no AS4_PATH for a 2-byte peer")
	}
	got := roundTrip(t, c, u).(*Update)
	if !reflect.DeepEqual(got.Attrs.ASPath, u.Attrs.ASPath) {
		t.Fatalf("path %v, want %v", got.Attrs.PathString(), u.Attrs.PathString())
	}
}

func TestMergeAS4KeepsLeadingTwoOctetHops(t *testing.T) {
	// A 2-byte speaker prepended 65001 in front of a path whose AS4_PATH
	// only covers the original two hops.
	path := []Segment{{ASNs: []uint32{65001, ASTrans, 64512}}}
	as4 := []Segment{{ASNs: []uint32{4200000001, 64512}}}
	got := (&Attrs{ASPath: mergeAS4(path, as4)}).PathString()
	if got != "65001 4200000001 64512" {
		t.Fatalf("got %q", got)
	}
}

func TestUpdateMissingAttributes(t *testing.T) {
	u := &Update{Announce: pfx("203.0.113.0/24"), NextHop: netip.MustParseAddr("192.0.2.1")}
	b, _ := Marshal(u)
	// Find the NEXT_HOP attribute and rename it to an unknown code.
	i := bytes.Index(b, []byte{FlagTransitive, AttrNextHop, 4})
	b[i+1] = 99
	_, err := Read(bytes.NewReader(b))
	var n *Notification
	if !errors.As(err, &n) || n.Subcode != ErrUpdateMissing || n.Data[0] != AttrNextHop {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateNeedsNextHops(t *testing.T) {
	if _, err := Marshal(&Update{Announce: pfx("203.0.113.0/24")}); err == nil {
		t.Error("IPv4 announce without a next hop marshalled")
	}
	if _, err := Marshal(&Update{Announce: pfx("2001:db8::/32"), NextHop: netip.MustParseAddr("192.0.2.1")}); err == nil {
		t.Error("IPv6 announce without an IPv6 next hop marshalled")
	}
}

func TestPrefixEncoding(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0/0":       "00",
		"10.0.0.0/8":      "080a",
		"192.0.2.128/25":  "19c0000280",
		"203.0.113.7/24":  "18cb0071", // host bits dropped
		"2001:db8::/32":   "2020010db8",
		"2001:db8:1::/48": "302001_0db80001",
	}
	for in, want := range cases {
		got := hex.EncodeToString(appendPrefix(nil, netip.MustParsePrefix(in)))
		if got != strings.ReplaceAll(want, "_", "") {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
	if _, err := parsePrefixes([]byte{33, 1, 2, 3, 4, 5}, false); err == nil {
		t.Error("a /33 IPv4 prefix parsed")
	}
	if _, err := parsePrefixes([]byte{24, 1, 2}, false); err == nil {
		t.Error("a truncated prefix parsed")
	}
}

func TestPrepend(t *testing.T) {
	a := &Attrs{}
	a.Prepend(64512, 1)
	a.Prepend(64512, 3)
	if a.PathString() != "64512 64512 64512 64512" || a.PathLen() != 4 || len(a.ASPath) != 1 {
		t.Fatalf("got %q in %d segments", a.PathString(), len(a.ASPath))
	}
	a.ASPath = append(a.ASPath, Segment{Set: true, ASNs: []uint32{1, 2}})
	if a.PathLen() != 5 || a.PathString() != "64512 64512 64512 64512 {1 2}" {
		t.Fatalf("got %q len %d", a.PathString(), a.PathLen())
	}
}

func TestCommunities(t *testing.T) {
	for _, s := range []string{"64512:100", "no-export", "graceful-shutdown", "0:0", "65535:65535"} {
		c, err := ParseCommunity(s)
		if err != nil || CommunityString(c) != s {
			// 65535:65535 has no name, so it reads back the same.
			t.Errorf("%s: got %q, %v", s, CommunityString(c), err)
		}
	}
	for _, s := range []string{"64512", "70000:1", "a:b"} {
		if _, err := ParseCommunity(s); err == nil {
			t.Errorf("%s parsed", s)
		}
	}
	lc, err := ParseLargeCommunity("4200000001:7:8")
	if err != nil || lc.String() != "4200000001:7:8" {
		t.Errorf("large: %v %v", lc, err)
	}
}

func FuzzRead(f *testing.F) {
	for _, m := range []Message{Keepalive{}, Shutdown("x"),
		&Open{AS: 64512, HoldTime: 90, ID: netip.MustParseAddr("10.0.0.1"), Families: []Family{IPv6Unicast}},
		&Update{Announce: pfx("2001:db8::/32", "10.0.0.0/8"), NextHop: netip.MustParseAddr("10.0.0.1"), NextHop6: netip.MustParseAddr("2001:db8::1")},
	} {
		b, _ := Marshal(m)
		f.Add(b, false)
	}
	f.Fuzz(func(t *testing.T, b []byte, two bool) {
		c := Codec{TwoOctetAS: two}
		m, err := c.Read(bytes.NewReader(b))
		if err != nil {
			return
		}
		// Whatever parses must marshal again.
		if _, err := c.Marshal(m); err != nil {
			var n *Notification
			if !errors.As(err, &n) && !strings.Contains(err.Error(), "next hop") && !strings.Contains(err.Error(), "bytes") && !strings.Contains(err.Error(), "router ID") {
				t.Fatalf("parsed %T but can't marshal: %v", m, err)
			}
		}
	})
}
