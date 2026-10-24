package bgp

import (
	"fmt"
	"net/netip"
)

// appendPrefix writes a prefix as NLRI: its length in bits, then only
// the bytes those bits cover.
func appendPrefix(b []byte, p netip.Prefix) []byte {
	p = p.Masked()
	n := (p.Bits() + 7) / 8
	b = append(b, byte(p.Bits()))
	return append(b, p.Addr().AsSlice()[:n]...)
}

// parsePrefixes reads NLRI for one address family. Host bits past the
// length are cleared rather than rejected, as most speakers do.
func parsePrefixes(b []byte, v6 bool) ([]netip.Prefix, error) {
	max := 32
	if v6 {
		max = 128
	}
	var out []netip.Prefix
	for len(b) > 0 {
		bits := int(b[0])
		n := (bits + 7) / 8
		if bits > max || 1+n > len(b) {
			return nil, fmt.Errorf("bgp: prefix length %d in %d bytes", bits, len(b)-1)
		}
		var a [16]byte
		copy(a[:], b[1:1+n])
		var addr netip.Addr
		if v6 {
			addr = netip.AddrFrom16(a)
		} else {
			addr = netip.AddrFrom4([4]byte(a[:4]))
		}
		out = append(out, netip.PrefixFrom(addr, bits).Masked())
		b = b[1+n:]
	}
	return out, nil
}
