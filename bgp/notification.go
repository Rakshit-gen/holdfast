package bgp

import "fmt"

// NOTIFICATION error codes and the subcodes holdfast sends or reads.
const (
	ErrHeader       = 1
	ErrOpen         = 2
	ErrUpdate       = 3
	ErrHoldTimer    = 4
	ErrFSM          = 5
	ErrCease        = 6
	ErrHeaderSync   = 1 // connection not synchronized
	ErrHeaderLength = 2
	ErrHeaderType   = 3

	ErrOpenVersion    = 1
	ErrOpenPeerAS     = 2
	ErrOpenID         = 3
	ErrOpenOptional   = 4
	ErrOpenHoldTime   = 6
	ErrOpenCapability = 7

	ErrUpdateAttrList   = 1
	ErrUpdateWellKnown  = 2
	ErrUpdateMissing    = 3
	ErrUpdateFlags      = 4
	ErrUpdateAttrLength = 5
	ErrUpdateOrigin     = 6
	ErrUpdateNextHop    = 8
	ErrUpdateOptional   = 9
	ErrUpdateNetwork    = 10
	ErrUpdateASPath     = 11

	// Cease subcodes, RFC 4486.
	CeaseMaxPrefixes = 1
	CeaseShutdown    = 2
	CeaseDeconfig    = 3
	CeaseReset       = 4
	CeaseRejected    = 5
	CeaseCollision   = 7
)

var codeNames = map[uint8]string{
	ErrHeader: "message header error", ErrOpen: "OPEN message error", ErrUpdate: "UPDATE message error",
	ErrHoldTimer: "hold timer expired", ErrFSM: "finite state machine error", ErrCease: "cease",
}

// Notification is a NOTIFICATION. It is also the error type for anything
// wrong with a received message, so the session can send it back.
type Notification struct {
	Code, Subcode uint8
	Data          []byte
}

// Type is TypeNotification.
func (*Notification) Type() uint8 { return TypeNotification }

func (n *Notification) body(b []byte, _ Codec) ([]byte, error) {
	b = append(b, n.Code, n.Subcode)
	return append(b, n.Data...), nil
}

func parseNotification(b []byte) (Message, error) {
	if len(b) < 2 {
		return nil, &Notification{Code: ErrHeader, Subcode: ErrHeaderLength, Data: lenData(HeaderLen + len(b))}
	}
	return &Notification{Code: b[0], Subcode: b[1], Data: append([]byte(nil), b[2:]...)}, nil
}

func (n *Notification) Error() string {
	name := codeNames[n.Code]
	if name == "" {
		name = fmt.Sprintf("code %d", n.Code)
	}
	s := fmt.Sprintf("bgp: %s, subcode %d", name, n.Subcode)
	if n.Code == ErrCease && (n.Subcode == CeaseShutdown || n.Subcode == CeaseReset) && len(n.Data) > 0 {
		// RFC 8203 shutdown communication: a length byte, then UTF-8.
		if l := int(n.Data[0]); l+1 <= len(n.Data) {
			s += fmt.Sprintf(": %q", n.Data[1:1+l])
		}
	}
	return s
}

// Shutdown is a Cease with an RFC 8203 message, up to 255 bytes, that the
// peer's operators see in their logs.
func Shutdown(msg string) *Notification {
	if len(msg) > 255 {
		msg = msg[:255]
	}
	return &Notification{Code: ErrCease, Subcode: CeaseShutdown, Data: append([]byte{byte(len(msg))}, msg...)}
}
