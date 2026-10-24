package bgp

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// Update is an UPDATE. IPv4 prefixes go in the classic withdrawn and NLRI
// fields, IPv6 ones in MP_REACH_NLRI and MP_UNREACH_NLRI, so one Update
// can carry both. Attrs apply to every announced prefix.
type Update struct {
	Withdraw []netip.Prefix
	Announce []netip.Prefix
	Attrs    Attrs
	NextHop  netip.Addr // for IPv4 prefixes
	NextHop6 netip.Addr // for IPv6 prefixes
	// endOfRIB marks an IPv6 End-of-RIB, an empty MP_UNREACH_NLRI. An
	// IPv4 End-of-RIB is simply an empty Update.
	endOfRIB6 bool
}

// EndOfRIB is the marker sent after the initial routes for a family,
// RFC 4724.
func EndOfRIB(f Family) *Update {
	return &Update{endOfRIB6: f == IPv6Unicast}
}

// IsEndOfRIB reports whether u is an End-of-RIB marker and for which
// family.
func (u *Update) IsEndOfRIB() (Family, bool) {
	if u.endOfRIB6 {
		return IPv6Unicast, true
	}
	if len(u.Withdraw) == 0 && len(u.Announce) == 0 && u.Attrs.ASPath == nil && len(u.Attrs.Other) == 0 {
		return IPv4Unicast, true
	}
	return Family{}, false
}

// Type is TypeUpdate.
func (*Update) Type() uint8 { return TypeUpdate }

func split(ps []netip.Prefix) (v4, v6 []netip.Prefix) {
	for _, p := range ps {
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	return
}

func (u *Update) body(b []byte, c Codec) ([]byte, error) {
	w4, w6 := split(u.Withdraw)
	a4, a6 := split(u.Announce)
	if len(a4) > 0 && !u.NextHop.Is4() {
		return nil, fmt.Errorf("bgp: announcing IPv4 prefixes needs an IPv4 next hop")
	}
	if len(a6) > 0 && !u.NextHop6.Is6() {
		return nil, fmt.Errorf("bgp: announcing IPv6 prefixes needs an IPv6 next hop")
	}

	var wd []byte
	for _, p := range w4 {
		wd = appendPrefix(wd, p)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(wd)))
	b = append(b, wd...)

	var at []byte
	if len(u.Announce) > 0 {
		a := &u.Attrs
		at = appendAttr(at, FlagTransitive, AttrOrigin, []byte{a.Origin})
		at = appendAttr(at, FlagTransitive, AttrASPath, appendPath(nil, a.ASPath, c.TwoOctetAS))
		if c.TwoOctetAS && hasBig(a.ASPath) {
			at = appendAttr(at, FlagOptional|FlagTransitive, AttrAS4Path, appendPath(nil, a.ASPath, false))
		}
		if len(a4) > 0 {
			nh := u.NextHop.As4()
			at = appendAttr(at, FlagTransitive, AttrNextHop, nh[:])
		}
		if a.HasMED {
			at = appendAttr(at, FlagOptional, AttrMED, binary.BigEndian.AppendUint32(nil, a.MED))
		}
		if a.HasLocalPref {
			at = appendAttr(at, FlagTransitive, AttrLocalPref, binary.BigEndian.AppendUint32(nil, a.LocalPref))
		}
		if a.AtomicAggregate {
			at = appendAttr(at, FlagTransitive, AttrAtomicAggregate, nil)
		}
		if len(a.Communities) > 0 {
			var v []byte
			for _, cm := range a.Communities {
				v = binary.BigEndian.AppendUint32(v, cm)
			}
			at = appendAttr(at, FlagOptional|FlagTransitive, AttrCommunities, v)
		}
		if len(a.LargeCommunities) > 0 {
			var v []byte
			for _, cm := range a.LargeCommunities {
				v = binary.BigEndian.AppendUint32(v, cm.Global)
				v = binary.BigEndian.AppendUint32(v, cm.Local1)
				v = binary.BigEndian.AppendUint32(v, cm.Local2)
			}
			at = appendAttr(at, FlagOptional|FlagTransitive, AttrLargeCommunity, v)
		}
		if len(a6) > 0 {
			v := binary.BigEndian.AppendUint16(nil, AFIIPv6)
			nh := u.NextHop6.As16()
			v = append(v, SAFIUnicast, 16)
			v = append(v, nh[:]...)
			v = append(v, 0) // reserved
			for _, p := range a6 {
				v = appendPrefix(v, p)
			}
			at = appendAttr(at, FlagOptional, AttrMPReach, v)
		}
		for _, r := range a.Other {
			at = appendAttr(at, r.Flags&^FlagExtLen, r.Code, r.Value)
		}
	}
	if len(w6) > 0 || u.endOfRIB6 {
		v := binary.BigEndian.AppendUint16(nil, AFIIPv6)
		v = append(v, SAFIUnicast)
		for _, p := range w6 {
			v = appendPrefix(v, p)
		}
		at = appendAttr(at, FlagOptional, AttrMPUnreach, v)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(at)))
	b = append(b, at...)
	for _, p := range a4 {
		b = appendPrefix(b, p)
	}
	return b, nil
}

func (c Codec) parseUpdate(b []byte) (Message, error) {
	malformed := &Notification{Code: ErrUpdate, Subcode: ErrUpdateAttrList}
	if len(b) < 4 {
		return nil, malformed
	}
	wl := int(binary.BigEndian.Uint16(b))
	if 2+wl+2 > len(b) {
		return nil, malformed
	}
	u := &Update{}
	var err error
	if u.Withdraw, err = parsePrefixes(b[2:2+wl], false); err != nil {
		return nil, malformed
	}
	b = b[2+wl:]
	al := int(binary.BigEndian.Uint16(b))
	if 2+al > len(b) {
		return nil, malformed
	}
	attrs, nlri := b[2:2+al], b[2+al:]
	if u.Announce, err = parsePrefixes(nlri, false); err != nil {
		return nil, &Notification{Code: ErrUpdate, Subcode: ErrUpdateNetwork}
	}

	seen := map[uint8]bool{}
	var as4 []Segment
	for len(attrs) > 0 {
		if len(attrs) < 3 {
			return nil, malformed
		}
		flags, code := attrs[0], attrs[1]
		hl, n := 3, int(attrs[2])
		if flags&FlagExtLen != 0 {
			if len(attrs) < 4 {
				return nil, malformed
			}
			hl, n = 4, int(binary.BigEndian.Uint16(attrs[2:]))
		}
		if hl+n > len(attrs) {
			return nil, &Notification{Code: ErrUpdate, Subcode: ErrUpdateAttrLength}
		}
		v := attrs[hl : hl+n]
		attrs = attrs[hl+n:]
		if seen[code] {
			return nil, malformed
		}
		seen[code] = true
		if err := c.parseAttr(u, &as4, flags, code, v); err != nil {
			return nil, err
		}
	}
	if as4 != nil {
		u.Attrs.ASPath = mergeAS4(u.Attrs.ASPath, as4)
	}
	if len(u.Announce) > 0 {
		for _, need := range []uint8{AttrOrigin, AttrASPath, AttrNextHop} {
			if !seen[need] {
				return nil, &Notification{Code: ErrUpdate, Subcode: ErrUpdateMissing, Data: []byte{need}}
			}
		}
	}
	return u, nil
}

func (c Codec) parseAttr(u *Update, as4 *[]Segment, flags, code uint8, v []byte) error {
	bad := func(sub uint8) error {
		return &Notification{Code: ErrUpdate, Subcode: sub, Data: []byte{flags, code, byte(len(v))}}
	}
	a := &u.Attrs
	switch code {
	case AttrOrigin:
		if len(v) != 1 {
			return bad(ErrUpdateAttrLength)
		}
		if v[0] > OriginIncomplete {
			return bad(ErrUpdateOrigin)
		}
		a.Origin = v[0]
	case AttrASPath:
		p, err := parsePath(v, c.TwoOctetAS)
		if err != nil {
			return err
		}
		a.ASPath = p
		if a.ASPath == nil {
			a.ASPath = []Segment{}
		}
	case AttrAS4Path:
		if !c.TwoOctetAS {
			return nil // a four-octet speaker ignores it
		}
		p, err := parsePath(v, false)
		if err != nil {
			return nil // RFC 6793: a bad AS4_PATH is discarded, not fatal
		}
		*as4 = p
	case AttrNextHop:
		if len(v) != 4 {
			return bad(ErrUpdateAttrLength)
		}
		u.NextHop = netip.AddrFrom4([4]byte(v))
	case AttrMED:
		if len(v) != 4 {
			return bad(ErrUpdateAttrLength)
		}
		a.MED, a.HasMED = binary.BigEndian.Uint32(v), true
	case AttrLocalPref:
		if len(v) != 4 {
			return bad(ErrUpdateAttrLength)
		}
		a.LocalPref, a.HasLocalPref = binary.BigEndian.Uint32(v), true
	case AttrAtomicAggregate:
		a.AtomicAggregate = true
	case AttrCommunities:
		if len(v)%4 != 0 {
			return bad(ErrUpdateAttrLength)
		}
		for i := 0; i < len(v); i += 4 {
			a.Communities = append(a.Communities, binary.BigEndian.Uint32(v[i:]))
		}
	case AttrLargeCommunity:
		if len(v)%12 != 0 {
			return bad(ErrUpdateAttrLength)
		}
		for i := 0; i < len(v); i += 12 {
			a.LargeCommunities = append(a.LargeCommunities, LargeCommunity{
				binary.BigEndian.Uint32(v[i:]), binary.BigEndian.Uint32(v[i+4:]), binary.BigEndian.Uint32(v[i+8:])})
		}
	case AttrMPReach:
		if len(v) < 5 {
			return bad(ErrUpdateOptional)
		}
		afi, safi, nhl := binary.BigEndian.Uint16(v), v[2], int(v[3])
		if len(v) < 4+nhl+1 {
			return bad(ErrUpdateOptional)
		}
		if afi != AFIIPv6 || safi != SAFIUnicast {
			a.Other = append(a.Other, RawAttr{flags, code, append([]byte(nil), v...)})
			return nil
		}
		// 16 bytes global, or 32 with a link-local after it.
		if nhl != 16 && nhl != 32 {
			return bad(ErrUpdateOptional)
		}
		u.NextHop6 = netip.AddrFrom16([16]byte(v[4:20]))
		ps, err := parsePrefixes(v[4+nhl+1:], true)
		if err != nil {
			return bad(ErrUpdateOptional)
		}
		u.Announce = append(u.Announce, ps...)
	case AttrMPUnreach:
		if len(v) < 3 {
			return bad(ErrUpdateOptional)
		}
		if binary.BigEndian.Uint16(v) != AFIIPv6 || v[2] != SAFIUnicast {
			a.Other = append(a.Other, RawAttr{flags, code, append([]byte(nil), v...)})
			return nil
		}
		ps, err := parsePrefixes(v[3:], true)
		if err != nil {
			return bad(ErrUpdateOptional)
		}
		if len(ps) == 0 {
			u.endOfRIB6 = true
		}
		u.Withdraw = append(u.Withdraw, ps...)
	default:
		a.Other = append(a.Other, RawAttr{flags, code, append([]byte(nil), v...)})
	}
	return nil
}
