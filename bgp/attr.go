package bgp

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Path attribute type codes.
const (
	AttrOrigin          = 1
	AttrASPath          = 2
	AttrNextHop         = 3
	AttrMED             = 4
	AttrLocalPref       = 5
	AttrAtomicAggregate = 6
	AttrAggregator      = 7
	AttrCommunities     = 8
	AttrMPReach         = 14
	AttrMPUnreach       = 15
	AttrAS4Path         = 17
	AttrLargeCommunity  = 32
)

// Attribute flag bits.
const (
	FlagOptional   = 0x80
	FlagTransitive = 0x40
	FlagPartial    = 0x20
	FlagExtLen     = 0x10
)

// Origin values.
const (
	OriginIGP        = 0
	OriginEGP        = 1
	OriginIncomplete = 2
)

// Well-known communities, RFC 1997 and RFC 8326.
const (
	CommunityGracefulShutdown = 0xffff0000
	CommunityNoExport         = 0xffffff01
	CommunityNoAdvertise      = 0xffffff02
)

// Segment is one AS_PATH segment.
type Segment struct {
	Set  bool // AS_SET rather than AS_SEQUENCE
	ASNs []uint32
}

// Attrs are the path attributes of an announcement. Next hops live on the
// Update, since IPv6 carries its own inside MP_REACH_NLRI.
type Attrs struct {
	Origin           uint8
	ASPath           []Segment
	MED              uint32
	HasMED           bool
	LocalPref        uint32
	HasLocalPref     bool
	AtomicAggregate  bool
	Communities      []uint32
	LargeCommunities []LargeCommunity
	// Other holds attributes holdfast doesn't interpret, so a received
	// route can be shown in full.
	Other []RawAttr
}

// LargeCommunity is an RFC 8092 community.
type LargeCommunity struct{ Global, Local1, Local2 uint32 }

func (c LargeCommunity) String() string { return fmt.Sprintf("%d:%d:%d", c.Global, c.Local1, c.Local2) }

// RawAttr is an attribute as it came off the wire.
type RawAttr struct {
	Flags, Code uint8
	Value       []byte
}

// PathLen is the AS_PATH length as best-path selection counts it: one per
// AS in a sequence, one per set.
func (a *Attrs) PathLen() int {
	n := 0
	for _, s := range a.ASPath {
		if s.Set {
			n++
		} else {
			n += len(s.ASNs)
		}
	}
	return n
}

// Prepend puts an AS at the front of the path n times.
func (a *Attrs) Prepend(as uint32, n int) {
	if n <= 0 {
		return
	}
	add := slices.Repeat([]uint32{as}, n)
	if len(a.ASPath) > 0 && !a.ASPath[0].Set && len(a.ASPath[0].ASNs)+n <= 255 {
		a.ASPath[0].ASNs = append(add, a.ASPath[0].ASNs...)
		return
	}
	a.ASPath = append([]Segment{{ASNs: add}}, a.ASPath...)
}

// PathString shows the path the way routers do: 64512 64512 {64513 64514}.
func (a *Attrs) PathString() string {
	var parts []string
	for _, s := range a.ASPath {
		var ns []string
		for _, n := range s.ASNs {
			ns = append(ns, strconv.FormatUint(uint64(n), 10))
		}
		if s.Set {
			parts = append(parts, "{"+strings.Join(ns, " ")+"}")
		} else {
			parts = append(parts, ns...)
		}
	}
	return strings.Join(parts, " ")
}

// CommunityString shows a community as high:low, or by name.
func CommunityString(c uint32) string {
	switch c {
	case CommunityGracefulShutdown:
		return "graceful-shutdown"
	case CommunityNoExport:
		return "no-export"
	case CommunityNoAdvertise:
		return "no-advertise"
	}
	return fmt.Sprintf("%d:%d", c>>16, c&0xffff)
}

// ParseCommunity reads high:low, or a well-known name.
func ParseCommunity(s string) (uint32, error) {
	switch s {
	case "graceful-shutdown":
		return CommunityGracefulShutdown, nil
	case "no-export":
		return CommunityNoExport, nil
	case "no-advertise":
		return CommunityNoAdvertise, nil
	}
	hi, lo, ok := strings.Cut(s, ":")
	h, err1 := strconv.ParseUint(hi, 10, 16)
	l, err2 := strconv.ParseUint(lo, 10, 16)
	if !ok || err1 != nil || err2 != nil {
		return 0, fmt.Errorf("community %q: want high:low with each part 0 to 65535", s)
	}
	return uint32(h)<<16 | uint32(l), nil
}

// ParseLargeCommunity reads global:local1:local2.
func ParseLargeCommunity(s string) (LargeCommunity, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return LargeCommunity{}, fmt.Errorf("large community %q: want global:local1:local2", s)
	}
	var v [3]uint32
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return LargeCommunity{}, fmt.Errorf("large community %q: %v", s, err)
		}
		v[i] = uint32(n)
	}
	return LargeCommunity{v[0], v[1], v[2]}, nil
}

func appendAttr(b []byte, flags, code uint8, v []byte) []byte {
	if len(v) > 255 {
		flags |= FlagExtLen
	}
	b = append(b, flags, code)
	if flags&FlagExtLen != 0 {
		b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	} else {
		b = append(b, byte(len(v)))
	}
	return append(b, v...)
}

// appendPath encodes AS_PATH segments with 2- or 4-byte numbers. With
// 2-byte numbers, any AS over 65535 becomes AS_TRANS.
func appendPath(b []byte, segs []Segment, two bool) []byte {
	for _, s := range segs {
		typ := byte(2)
		if s.Set {
			typ = 1
		}
		b = append(b, typ, byte(len(s.ASNs)))
		for _, n := range s.ASNs {
			if two {
				if n > 0xffff {
					n = ASTrans
				}
				b = binary.BigEndian.AppendUint16(b, uint16(n))
			} else {
				b = binary.BigEndian.AppendUint32(b, n)
			}
		}
	}
	return b
}

func parsePath(b []byte, two bool) ([]Segment, error) {
	size := 4
	if two {
		size = 2
	}
	var out []Segment
	for len(b) > 0 {
		if len(b) < 2 || (b[0] != 1 && b[0] != 2) {
			return nil, &Notification{Code: ErrUpdate, Subcode: ErrUpdateASPath}
		}
		n := int(b[1])
		if len(b) < 2+n*size {
			return nil, &Notification{Code: ErrUpdate, Subcode: ErrUpdateASPath}
		}
		s := Segment{Set: b[0] == 1, ASNs: make([]uint32, n)}
		for i := range n {
			if two {
				s.ASNs[i] = uint32(binary.BigEndian.Uint16(b[2+i*2:]))
			} else {
				s.ASNs[i] = binary.BigEndian.Uint32(b[2+i*4:])
			}
		}
		out = append(out, s)
		b = b[2+n*size:]
	}
	return out, nil
}

// hasBig says whether any AS in the path needs four bytes.
func hasBig(segs []Segment) bool {
	for _, s := range segs {
		for _, n := range s.ASNs {
			if n > 0xffff {
				return true
			}
		}
	}
	return false
}

// mergeAS4 rebuilds the real path from a 2-byte AS_PATH and an AS4_PATH,
// RFC 6793 section 4.2.3: AS4_PATH replaces the tail of AS_PATH it covers.
// If AS4_PATH is the longer one it is ignored.
func mergeAS4(path, as4 []Segment) []Segment {
	n, n4 := (&Attrs{ASPath: path}).PathLen(), (&Attrs{ASPath: as4}).PathLen()
	if n4 > n {
		return path
	}
	keep := n - n4
	var out []Segment
	for _, s := range path {
		if keep <= 0 {
			break
		}
		if s.Set {
			out = append(out, s)
			keep--
			continue
		}
		take := min(keep, len(s.ASNs))
		out = append(out, Segment{ASNs: s.ASNs[:take]})
		keep -= take
	}
	return append(out, as4...)
}
