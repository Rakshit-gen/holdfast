package health

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// Probe repeats a check and turns its results into up or down.
type Probe struct {
	Name     string
	Check    Check
	Interval time.Duration // 0 means 2s
	Timeout  time.Duration // 0 means Interval
	Rise     int           // passes in a row to go up; 0 means 2
	Fall     int           // failures in a row to go down; 0 means 3
	Damping  *Damping      // nil means none
}

// Status is a probe's current verdict.
type Status struct {
	Name    string
	Up      bool
	Since   time.Time
	Streak  int // passes or failures in a row, matching the last result
	LastErr string
	// Held is set while damping holds the probe down although it passes.
	Held      bool
	HeldUntil time.Time
	Checks    int
}

// tracker is the pure part of a probe: results in, verdict out. A probe
// starts down, so a site announces nothing until its checks have passed.
type tracker struct {
	p       Probe
	st      Status
	healthy bool // rise and fall applied, before damping
	pass    bool // the last raw result
	damp    *damper
}

func newTracker(p Probe, now time.Time) *tracker {
	if p.Rise <= 0 {
		p.Rise = 2
	}
	if p.Fall <= 0 {
		p.Fall = 3
	}
	t := &tracker{p: p, st: Status{Name: p.Name, Since: now}}
	if p.Damping != nil {
		t.damp = &damper{d: *p.Damping}
	}
	return t
}

// observe records one result and reports whether Up changed.
func (t *tracker) observe(now time.Time, err error) bool {
	t.st.Checks++
	pass := err == nil
	if pass == t.pass && t.st.Checks > 1 {
		t.st.Streak++
	} else {
		t.st.Streak = 1
	}
	t.pass = pass
	if err != nil {
		t.st.LastErr = err.Error()
	}
	switch {
	case t.healthy && !pass && t.st.Streak >= t.p.Fall:
		t.healthy = false
		if t.damp != nil {
			t.damp.flap(now)
		}
	case !t.healthy && pass && t.st.Streak >= t.p.Rise:
		t.healthy = true
	}
	t.st.Held = t.healthy && t.damp != nil && t.damp.held(now)
	if t.st.Held {
		t.st.HeldUntil = t.damp.until(now)
	}
	up := t.healthy && !t.st.Held
	if up == t.st.Up {
		return false
	}
	t.st.Up, t.st.Since = up, now
	return true
}

// Prober runs probes and calls OnChange whenever one goes up or down.
type Prober struct {
	OnChange func(Status)

	mu sync.Mutex
	ts map[string]*tracker
}

// Status returns every probe's status.
func (p *Prober) Status() []Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Status, 0, len(p.ts))
	for _, t := range p.ts {
		out = append(out, t.st)
	}
	slices.SortFunc(out, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Run runs one probe until ctx ends.
func (p *Prober) Run(ctx context.Context, pr Probe) {
	if pr.Interval <= 0 {
		pr.Interval = 2 * time.Second
	}
	if pr.Timeout <= 0 || pr.Timeout > pr.Interval {
		pr.Timeout = pr.Interval
	}
	t := newTracker(pr, time.Now())
	p.mu.Lock()
	if p.ts == nil {
		p.ts = map[string]*tracker{}
	}
	p.ts[pr.Name] = t
	p.mu.Unlock()

	tick := time.NewTicker(pr.Interval)
	defer tick.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, pr.Timeout)
		err := pr.Check.Check(cctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		changed := t.observe(time.Now(), err)
		st := t.st
		p.mu.Unlock()
		if changed && p.OnChange != nil {
			p.OnChange(st)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
