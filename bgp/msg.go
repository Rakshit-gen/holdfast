// Package bgp is the part of BGP-4 (RFC 4271) a route announcer needs:
// the wire format of the four message types, the capabilities in OPEN,
// the path attributes in UPDATE, and a session state machine.
package bgp

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Message types.
const (
	TypeOpen         = 1
	TypeUpdate       = 2
	TypeNotification = 3
	TypeKeepalive    = 4
)

// HeaderLen is the marker, length and type.
const HeaderLen = 19

// MaxLen is the largest message RFC 4271 allows without the extended
// message capability.
const MaxLen = 4096

// Message is any of the four message types.
type Message interface {
	Type() uint8
	// body appends the message after the header.
	body(b []byte, c Codec) ([]byte, error)
}

// Codec carries what the session negotiated that changes the encoding.
// The zero Codec is a speaker with four-octet AS numbers, which is what
// holdfast always offers.
type Codec struct {
	// TwoOctetAS is set when the peer didn't offer the four-octet AS
	// capability: AS_PATH then carries 2-byte numbers, RFC 6793.
	TwoOctetAS bool
}

// Marshal encodes a message with the zero Codec.
func Marshal(m Message) ([]byte, error) { return Codec{}.Marshal(m) }

// Read reads a message with the zero Codec.
func Read(r io.Reader) (Message, error) { return Codec{}.Read(r) }

// Parse decodes a body with the zero Codec.
func Parse(typ uint8, body []byte) (Message, error) { return Codec{}.Parse(typ, body) }

// Marshal encodes a message with its header.
func (c Codec) Marshal(m Message) ([]byte, error) {
	b := make([]byte, HeaderLen, 64)
	for i := 0; i < 16; i++ {
		b[i] = 0xff
	}
	b[18] = m.Type()
	b, err := m.body(b, c)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxLen {
		return nil, fmt.Errorf("bgp: %d byte message is over %d", len(b), MaxLen)
	}
	binary.BigEndian.PutUint16(b[16:], uint16(len(b)))
	return b, nil
}

// Read reads one message. A malformed message comes back as a
// *Notification error, ready to send to the peer before closing.
func (c Codec) Read(r io.Reader) (Message, error) {
	var h [HeaderLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	for _, c := range h[:16] {
		if c != 0xff {
			return nil, &Notification{Code: ErrHeader, Subcode: ErrHeaderSync}
		}
	}
	n := int(binary.BigEndian.Uint16(h[16:]))
	if n < HeaderLen || n > MaxLen {
		return nil, &Notification{Code: ErrHeader, Subcode: ErrHeaderLength, Data: h[16:18]}
	}
	body := make([]byte, n-HeaderLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return c.Parse(h[18], body)
}

// Parse decodes a message body of the given type.
func (c Codec) Parse(typ uint8, body []byte) (Message, error) {
	switch typ {
	case TypeOpen:
		return parseOpen(body)
	case TypeUpdate:
		return c.parseUpdate(body)
	case TypeNotification:
		return parseNotification(body)
	case TypeKeepalive:
		if len(body) != 0 {
			return nil, &Notification{Code: ErrHeader, Subcode: ErrHeaderLength, Data: lenData(HeaderLen + len(body))}
		}
		return Keepalive{}, nil
	}
	return nil, &Notification{Code: ErrHeader, Subcode: ErrHeaderType, Data: []byte{typ}}
}

func lenData(n int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(n)) }

// Keepalive is a KEEPALIVE: a header and nothing else.
type Keepalive struct{}

// Type is TypeKeepalive.
func (Keepalive) Type() uint8 { return TypeKeepalive }

func (Keepalive) body(b []byte, _ Codec) ([]byte, error) { return b, nil }
