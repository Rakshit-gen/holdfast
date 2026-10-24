// Package sim plays a fleet of anycast sites through failures, minute by
// minute, and counts how much user traffic fails under each policy. The
// world is invented: failure rates, durations and how traffic moves are
// parameters, not measurements.
package sim

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/Rakshit-gen/holdfast/guard"
)

// Policy is how sites react to failing checks.
type Policy int

// Policies.
const (
	// Static never withdraws: plain anycast with no health checks.
	Static Policy = iota
	// Unguarded withdraws whenever the site's check fails.
	Unguarded
	// Guarded is holdfast: withdraw only down to the floor, and fail
	// static when the fleet can't be seen.
	Guarded
)

var policyNames = []string{"static", "unguarded", "guarded"}

func (p Policy) String() string { return policyNames[p] }

// Policies lists them in order.
var Policies = []Policy{Static, Unguarded, Guarded}

// Config is one world.
type Config struct {
	Sites   int
	Minutes int
	Seed    uint64

	// SiteFailRate is real failures per site per day: the service at
	// that site is broken and its check correctly says so.
	SiteFailRate float64
	SiteFailMins float64 // mean length

	// FalseRate is fleet-wide false alarms per day: something every
	// check depends on breaks (a shared upstream, a bad check config
	// pushed everywhere), so every check fails while the service works.
	FalseRate float64
	FalseMins float64

	// RegionRate is regional failures per day, taking a share of the
	// sites down for real at once.
	RegionRate  float64
	RegionShare float64
	RegionMins  float64

	// PartitionRate is per site per day: the site loses sight of the
	// other sites for a while (its own check may still pass or fail).
	PartitionRate float64
	PartitionMins float64

	// Floor is the guard's MinFraction.
	Floor float64

	// Leak is the share of a degraded site's own users that still reach
	// it: a longer path doesn't beat every route their network has.
	Leak float64

	// Script adds outages at fixed times, on top of the random ones.
	Script []Outage
}

// Outage is one scripted failure. A false alarm fails every check while
// the service works; otherwise Share of the sites, a run of neighbours
// starting at the first, really break.
type Outage struct {
	Start, Minutes int
	False          bool
	Share          float64
}

// Default is a mixed world: rare real failures at single sites, a false
// alarm about every 10 days, and a regional outage about every 20.
func Default() Config {
	return Config{
		Sites: 20, Minutes: 7 * 24 * 60,
		SiteFailRate: 0.2, SiteFailMins: 20,
		FalseRate: 0.1, FalseMins: 30,
		RegionRate: 0.05, RegionShare: 0.3, RegionMins: 45,
		PartitionRate: 0.05, PartitionMins: 15,
		Floor: 0.3, Leak: 0.2,
	}
}

// Result is one policy in one world.
type Result struct {
	Policy Policy
	// Failed is the share of user-minutes that reached a broken site or
	// no site at all.
	Failed float64
	// Dark is minutes in which no site announced.
	Dark int
	// Withdrawals counts announce-to-withdraw changes.
	Withdrawals int
}

type event struct{ start, end, site int } // site -1 means every site

// world draws every failure up front, so every policy faces the same ones.
type world struct {
	c        Config
	broken   [][]bool // [minute][site]: the service really doesn't work
	falseAll []bool   // [minute]: every check fails although service works
	cut      [][]bool // [minute][site]: site can't see the fleet
}

func poissonEvents(r *rand.Rand, perDay float64, minutes int, meanLen float64) []event {
	var out []event
	if perDay <= 0 {
		return nil
	}
	rate := perDay / 1440
	for t := 0.0; ; {
		t += r.ExpFloat64() / rate
		if int(t) >= minutes {
			return out
		}
		l := max(1, int(r.ExpFloat64()*meanLen))
		out = append(out, event{int(t), int(t) + l, 0})
	}
}

func newWorld(c Config) *world {
	r := rand.New(rand.NewPCG(c.Seed, 0x686f6c64))
	w := &world{c: c}
	w.broken = make([][]bool, c.Minutes)
	w.cut = make([][]bool, c.Minutes)
	for m := range c.Minutes {
		w.broken[m] = make([]bool, c.Sites)
		w.cut[m] = make([]bool, c.Sites)
	}
	w.falseAll = make([]bool, c.Minutes)
	mark := func(grid [][]bool, e event, site int) {
		for m := e.start; m < min(e.end, c.Minutes); m++ {
			grid[m][site] = true
		}
	}
	for s := range c.Sites {
		for _, e := range poissonEvents(r, c.SiteFailRate, c.Minutes, c.SiteFailMins) {
			mark(w.broken, e, s)
		}
		for _, e := range poissonEvents(r, c.PartitionRate, c.Minutes, c.PartitionMins) {
			mark(w.cut, e, s)
		}
	}
	for _, e := range poissonEvents(r, c.RegionRate, c.Minutes, c.RegionMins) {
		// A region is a run of neighbouring sites.
		n := max(1, int(c.RegionShare*float64(c.Sites)))
		first := r.IntN(c.Sites)
		for i := range n {
			mark(w.broken, e, (first+i)%c.Sites)
		}
	}
	for _, e := range poissonEvents(r, c.FalseRate, c.Minutes, c.FalseMins) {
		for m := e.start; m < min(e.end, c.Minutes); m++ {
			w.falseAll[m] = true
		}
	}
	for _, o := range c.Script {
		e := event{o.Start, o.Start + o.Minutes, 0}
		if o.False {
			for m := e.start; m < min(e.end, c.Minutes); m++ {
				w.falseAll[m] = true
			}
			continue
		}
		for s := range max(1, int(o.Share*float64(c.Sites))) {
			mark(w.broken, e, s%c.Sites)
		}
	}
	return w
}

// Run plays one policy through the world drawn from c.
func Run(c Config, p Policy) Result {
	return newWorld(c).run(p, nil)
}

// Minute is the fleet in one minute of a Timeline.
type Minute struct {
	Announce, Degrade, Withdraw int
	Failed                      float64
}

// Timeline plays one policy and returns every minute.
func Timeline(c Config, p Policy) []Minute {
	out := make([]Minute, 0, c.Minutes)
	newWorld(c).run(p, func(mode []guard.Mode, failed float64) {
		var m Minute
		for _, md := range mode {
			switch md {
			case guard.Announce:
				m.Announce++
			case guard.Degrade:
				m.Degrade++
			default:
				m.Withdraw++
			}
		}
		m.Failed = failed
		out = append(out, m)
	})
	return out
}

func (w *world) run(p Policy, each func(mode []guard.Mode, failed float64)) Result {
	c := w.c
	names := make([]string, c.Sites)
	for i := range names {
		names[i] = fmt.Sprintf("site%02d", i)
	}
	mode := make([]guard.Mode, c.Sites)
	since := make([]time.Time, c.Sites)
	// Every site starts announcing.
	reports := make([]guard.Report, c.Sites) // what each site said last minute
	t0 := time.Unix(0, 0)
	for i := range reports {
		since[i] = t0
		reports[i] = guard.Report{Site: names[i], Healthy: true, At: t0}
	}
	pol := guard.Policy{MinFraction: c.Floor, Stale: 90 * time.Second, Unguarded: p == Unguarded}
	res := Result{Policy: p}
	var failed float64
	next := make([]guard.Mode, c.Sites)
	for m := range c.Minutes {
		now := t0.Add(time.Duration(m) * time.Minute)
		for s := range c.Sites {
			healthy := !w.broken[m][s] && !w.falseAll[m]
			if p == Static {
				next[s] = guard.Announce
				continue
			}
			// The site sees last minute's reports, or, cut off, none
			// newer than when it was cut.
			view := reports
			if w.cut[m][s] {
				view = nil
			}
			next[s] = guard.Decide(guard.Input{
				Service: "svc", Site: names[s], Healthy: healthy, Mode: mode[s], Since: since[s],
				Fleet: view, Sites: names, Now: now,
			}, pol).Mode
		}
		for s := range c.Sites {
			if mode[s] != guard.Withdraw && next[s] == guard.Withdraw {
				res.Withdrawals++
			}
			if next[s] != mode[s] {
				mode[s], since[s] = next[s], now
			}
			// A cut-off site can't tell the others anything new either.
			if !w.cut[m][s] {
				reports[s] = guard.Report{Site: names[s], Healthy: !w.broken[m][s] && !w.falseAll[m], Mode: mode[s], At: now}
			}
		}
		f, dark := w.traffic(m, mode)
		if each != nil {
			each(mode, f)
		}
		failed += f
		if dark {
			res.Dark++
		}
	}
	res.Failed = failed / float64(c.Minutes)
	return res
}

// traffic sends each site's users somewhere for one minute. Users go to
// their own site if it announces normally. Otherwise they spread evenly
// over the sites announcing normally, or, if there are none, over the
// degraded ones: a longer path loses to any normal one but beats nothing.
// A degraded site still keeps Leak of its own users.
// It returns the share of users who failed, at a broken site or for lack
// of any, and whether no site announced at all.
func (w *world) traffic(m int, mode []guard.Mode) (failed float64, dark bool) {
	n := len(mode)
	var plain, degraded []int
	for s, md := range mode {
		switch md {
		case guard.Announce:
			plain = append(plain, s)
		case guard.Degrade:
			degraded = append(degraded, s)
		}
	}
	pool := plain
	if len(pool) == 0 {
		pool = degraded
	}
	if len(pool) == 0 {
		return 1, true
	}
	brokenIn := func(ss []int) float64 {
		k := 0
		for _, s := range ss {
			if w.broken[m][s] {
				k++
			}
		}
		return float64(k) / float64(len(ss))
	}
	spread := brokenIn(pool)
	for s := range n {
		var own float64
		if w.broken[m][s] {
			own = 1
		}
		var bad float64
		switch mode[s] {
		case guard.Announce:
			bad = own
		case guard.Degrade:
			bad = w.c.Leak*own + (1-w.c.Leak)*spread
		default:
			bad = spread
		}
		failed += bad / float64(n)
	}
	return failed, false
}
