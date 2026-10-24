package bgp

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// ASTrans is the 2-byte AS sent in OPEN by a speaker whose AS doesn't
// fit, RFC 6793.
const ASTrans = 23456

// Address families.
const (
	AFIIPv4     = 1
	AFIIPv6     = 2
	SAFIUnicast = 1
)

// Capability codes.
const (
	CapMultiprotocol   = 1
	CapRouteRefresh    = 2
	CapGracefulRestart = 64
	CapFourOctetAS     = 65
)

// Family is an AFI/SAFI pair.
type Family struct {
	AFI  uint16
	SAFI uint8
}

// IPv4 and IPv6 unicast.
var (
	IPv4Unicast = Family{AFIIPv4, SAFIUnicast}
	IPv6Unicast = Family{AFIIPv6, SAFIUnicast}
)

func (f Family) String() string {
	switch f {
	case IPv4Unicast:
		return "ipv4-unicast"
	case IPv6Unicast:
		return "ipv6-unicast"
	}
	return fmt.Sprintf("afi%d-safi%d", f.AFI, f.SAFI)
}

// Open is an OPEN with the capabilities holdfast understands pulled out.
// Capabilities it doesn't know are kept in Other.
type Open struct {
	AS       uint32 // the real AS, from the four-octet capability if present
	HoldTime uint16 // seconds; 0 or at least 3
	ID       netip.Addr

	Families     []Family
	FourOctet    bool
	RouteRefresh bool
	// GracefulRestart is set when the capability is present; Restart is
	// its restart time in seconds.
	GracefulRestart bool
	Restart         uint16
	Other           []Capability
}

// Capability is one capability TLV.
type Capability struct {
	Code  uint8
	Value []byte
}

// Type is TypeOpen.
func (*Open) Type() uint8 { return TypeOpen }

func (o *Open) body(b []byte, _ Codec) ([]byte, error) {
	if !o.ID.Is4() {
		return nil, fmt.Errorf("bgp: router ID %v is not IPv4", o.ID)
	}
	as2 := o.AS
	if as2 > 0xffff {
		as2 = ASTrans
	}
	b = append(b, 4)
	b = binary.BigEndian.AppendUint16(b, uint16(as2))
	b = binary.BigEndian.AppendUint16(b, o.HoldTime)
	id := o.ID.As4()
	b = append(b, id[:]...)

	var caps []Capability
	for _, f := range o.Families {
		v := binary.BigEndian.AppendUint16(nil, f.AFI)
		caps = append(caps, Capability{CapMultiprotocol, append(v, 0, f.SAFI)})
	}
	if o.RouteRefresh {
		caps = append(caps, Capability{CapRouteRefresh, nil})
	}
	if o.GracefulRestart {
		caps = append(caps, Capability{CapGracefulRestart, binary.BigEndian.AppendUint16(nil, o.Restart&0x0fff)})
	}
	if o.FourOctet {
		caps = append(caps, Capability{CapFourOctetAS, binary.BigEndian.AppendUint32(nil, o.AS)})
	}
	caps = append(caps, o.Other...)

	// One optional parameter of type 2 holding every capability.
	var param []byte
	for _, c := range caps {
		if len(c.Value) > 255 {
			return nil, fmt.Errorf("bgp: capability %d is %d bytes", c.Code, len(c.Value))
		}
		param = append(param, c.Code, byte(len(c.Value)))
		param = append(param, c.Value...)
	}
	if len(param) == 0 {
		return append(b, 0), nil
	}
	if len(param) > 253 {
		return nil, fmt.Errorf("bgp: %d bytes of capabilities", len(param))
	}
	b = append(b, byte(len(param)+2), 2, byte(len(param)))
	return append(b, param...), nil
}

func parseOpen(b []byte) (Message, error) {
	if len(b) < 10 {
		return nil, &Notification{Code: ErrHeader, Subcode: ErrHeaderLength, Data: lenData(HeaderLen + len(b))}
	}
	if b[0] != 4 {
		return nil, &Notification{Code: ErrOpen, Subcode: ErrOpenVersion, Data: []byte{0, 4}}
	}
	o := &Open{
		AS:       uint32(binary.BigEndian.Uint16(b[1:])),
		HoldTime: binary.BigEndian.Uint16(b[3:]),
		ID:       netip.AddrFrom4([4]byte(b[5:9])),
	}
	if o.HoldTime == 1 || o.HoldTime == 2 {
		return nil, &Notification{Code: ErrOpen, Subcode: ErrOpenHoldTime}
	}
	if o.ID == netip.IPv4Unspecified() {
		return nil, &Notification{Code: ErrOpen, Subcode: ErrOpenID}
	}
	params := b[10:]
	if int(b[9]) != len(params) {
		return nil, &Notification{Code: ErrOpen, Subcode: 0}
	}
	for len(params) > 0 {
		if len(params) < 2 || int(params[1])+2 > len(params) {
			return nil, &Notification{Code: ErrOpen, Subcode: 0}
		}
		typ, val := params[0], params[2:2+params[1]]
		params = params[2+params[1]:]
		if typ != 2 {
			return nil, &Notification{Code: ErrOpen, Subcode: ErrOpenOptional}
		}
		for len(val) > 0 {
			if len(val) < 2 || int(val[1])+2 > len(val) {
				return nil, &Notification{Code: ErrOpen, Subcode: 0}
			}
			c := Capability{Code: val[0], Value: append([]byte(nil), val[2:2+val[1]]...)}
			val = val[2+val[1]:]
			if err := o.addCap(c); err != nil {
				return nil, err
			}
		}
	}
	return o, nil
}

func (o *Open) addCap(c Capability) error {
	bad := &Notification{Code: ErrOpen, Subcode: 0}
	switch c.Code {
	case CapMultiprotocol:
		if len(c.Value) != 4 {
			return bad
		}
		o.Families = append(o.Families, Family{binary.BigEndian.Uint16(c.Value), c.Value[3]})
	case CapRouteRefresh:
		o.RouteRefresh = true
	case CapGracefulRestart:
		if len(c.Value) < 2 {
			return bad
		}
		o.GracefulRestart = true
		o.Restart = binary.BigEndian.Uint16(c.Value) & 0x0fff
	case CapFourOctetAS:
		if len(c.Value) != 4 {
			return bad
		}
		o.FourOctet = true
		o.AS = binary.BigEndian.Uint32(c.Value)
	default:
		o.Other = append(o.Other, c)
	}
	return nil
}

// Supports says whether the OPEN offered a family. A peer that sent no
// multiprotocol capability at all speaks IPv4 unicast only, RFC 4760.
func (o *Open) Supports(f Family) bool {
	if len(o.Families) == 0 {
		return f == IPv4Unicast
	}
	for _, g := range o.Families {
		if g == f {
			return true
		}
	}
	return false
}
