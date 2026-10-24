package health

import (
	"math"
	"time"
)

// Damping holds a flapping check down, in the style of BGP route flap
// damping (RFC 2439). Each time the check goes down it gains Penalty.
// The penalty halves every HalfLife. Above Suppress the check counts as
// down even while it passes, until the penalty decays below Reuse.
type Damping struct {
	Penalty  float64
	HalfLife time.Duration
	Suppress float64
	Reuse    float64
	// Max caps the penalty, which caps how long a check stays held down:
	// HalfLife * log2(Max/Reuse).
	Max float64
}

// DefaultDamping holds a check down after its third quick flap, and for
// at most about 20 minutes.
var DefaultDamping = Damping{Penalty: 1000, HalfLife: 5 * time.Minute, Suppress: 2500, Reuse: 750, Max: 12000}

// damper tracks one check's penalty.
type damper struct {
	d          Damping
	penalty    float64
	at         time.Time
	suppressed bool
}

func (m *damper) decay(now time.Time) {
	if !m.at.IsZero() && m.d.HalfLife > 0 {
		m.penalty *= math.Exp2(-now.Sub(m.at).Seconds() / m.d.HalfLife.Seconds())
	}
	m.at = now
	if m.suppressed && m.penalty < m.d.Reuse {
		m.suppressed = false
	}
}

// flap records a fall.
func (m *damper) flap(now time.Time) {
	m.decay(now)
	m.penalty = math.Min(m.penalty+m.d.Penalty, m.d.Max)
	if m.penalty >= m.d.Suppress {
		m.suppressed = true
	}
}

// held reports whether the check is held down now.
func (m *damper) held(now time.Time) bool {
	m.decay(now)
	return m.suppressed
}

// until is when a held check will be released if it doesn't flap again.
func (m *damper) until(now time.Time) time.Time {
	m.decay(now)
	if !m.suppressed || m.penalty <= 0 {
		return now
	}
	d := m.d.HalfLife.Seconds() * math.Log2(m.penalty/m.d.Reuse)
	return now.Add(time.Duration(d * float64(time.Second)))
}
