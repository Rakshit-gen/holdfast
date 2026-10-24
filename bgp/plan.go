package bgp

import (
	"net/netip"
	"slices"
	"strings"
)

// Plan turns a route change into as few UPDATEs as fit: withdrawals
// packed together, and announcements grouped by identical attributes and
// next hop. Toward an eBGP peer our AS goes on the front of the path;
// toward iBGP the path is left alone and LOCAL_PREF defaults to 100.
func Plan(withdraw []netip.Prefix, announce []Route, localAS, peerAS uint32) []*Update {
	var out []*Update
	ebgp := localAS != peerAS

	slices.SortFunc(withdraw, comparePrefix)
	for _, chunk := range chunks(withdraw, 0) {
		out = append(out, &Update{Withdraw: chunk})
	}

	groups := map[string][]Route{}
	var keys []string
	for _, r := range announce {
		a := r.Attrs
		a.ASPath = clonePath(a.ASPath)
		if ebgp {
			a.Prepend(localAS, 1)
			a.HasLocalPref = false
		} else if !a.HasLocalPref {
			a.LocalPref, a.HasLocalPref = 100, true
		}
		r.Attrs = a
		k := attrKey(&a) + "|" + r.NextHop.String()
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	slices.Sort(keys)
	for _, k := range keys {
		rs := groups[k]
		ps := make([]netip.Prefix, len(rs))
		for i, r := range rs {
			ps[i] = r.Prefix
		}
		slices.SortFunc(ps, comparePrefix)
		head := rs[0]
		for _, chunk := range chunks(ps, attrSize(&head.Attrs)) {
			u := &Update{Announce: chunk, Attrs: head.Attrs}
			if head.NextHop.Is4() {
				u.NextHop = head.NextHop
			} else {
				u.NextHop6 = head.NextHop
			}
			out = append(out, u)
		}
	}
	return out
}

func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return a.Bits() - b.Bits()
}

func clonePath(p []Segment) []Segment {
	out := make([]Segment, len(p))
	for i, s := range p {
		out[i] = Segment{Set: s.Set, ASNs: slices.Clone(s.ASNs)}
	}
	return out
}

// chunks splits prefixes so each UPDATE stays under MaxLen, leaving room
// for attrs bytes of attributes. A prefix takes at most 17 bytes.
func chunks(ps []netip.Prefix, attrs int) [][]netip.Prefix {
	room := MaxLen - HeaderLen - 4 - attrs - 64 // 64 for MP_REACH framing and next hops
	per := max(room/17, 1)
	var out [][]netip.Prefix
	for len(ps) > 0 {
		n := min(per, len(ps))
		out = append(out, ps[:n])
		ps = ps[n:]
	}
	return out
}

func attrSize(a *Attrs) int {
	u := &Update{Announce: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, Attrs: *a, NextHop: netip.IPv4Unspecified()}
	b, err := u.body(nil, Codec{TwoOctetAS: true})
	if err != nil {
		return 1024
	}
	// The 2-byte encoding can add AS4_PATH, so it is the larger one.
	return len(b)
}

// attrKey is a string that is equal for equal attributes.
func attrKey(a *Attrs) string {
	u := &Update{Announce: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, Attrs: *a, NextHop: netip.IPv4Unspecified()}
	b, _ := u.body(nil, Codec{})
	var sb strings.Builder
	sb.Write(b)
	return sb.String()
}
