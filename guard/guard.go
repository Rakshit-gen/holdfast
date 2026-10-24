// Package guard decides whether a site may withdraw a service's anycast
// prefixes when its health checks fail.
//
// Withdrawing a sick site is the point of health-checked anycast. The
// danger is every site withdrawing at once: when the checks share a
// dependency that breaks, or a bad config or a mass drain reaches the
// whole fleet, each site does the locally right thing and the service
// disappears from the internet. This is how Facebook's DNS went dark on
// 2021-10-04: the DNS sites withdrew their routes because they couldn't
// reach the data centers, all at the same time.
//
// The guard adds two rules. A floor: no more than enough sites to leave
// MinUp of them announcing may withdraw; the rest stay announced with a
// longer AS path, so traffic leaves them but has somewhere to go if
// nothing else is left. And fail static: a site that can't see enough of
// the fleet to know where the floor is doesn't withdraw at all.
package guard

import (
	"cmp"
	"hash/fnv"
	"math"
	"slices"
	"time"
)

// Mode is how a site announces a service.
type Mode int

// Modes, from best to worst for traffic.
const (
	Announce Mode = iota // normal
	Degrade              // announced with a longer AS path
	Withdraw             // not announced
)

var modeNames = []string{"announce", "degrade", "withdraw"}

func (m Mode) String() string { return modeNames[m] }

// Report is what one site says about one service.
type Report struct {
	Site    string
	Healthy bool
	Mode    Mode
	At      time.Time // when the site said it
}

// Policy sets the floor and how fresh reports must be.
type Policy struct {
	// MinUp is how many sites must keep announcing, degraded or not. If
	// MinFraction gives more, that wins.
	MinUp       int
	MinFraction float64
	// Stale is how old a report can be before the site counts as
	// unknown. 0 means 30 seconds.
	Stale time.Duration
	// Quorum is the share of the other sites a site must have fresh
	// reports from before it will withdraw. 0 means 0.5.
	Quorum float64
	// Unguarded withdraws whenever health fails, like a plain health
	// checker. It exists to compare against.
	Unguarded bool
}

func (p Policy) stale() time.Duration {
	if p.Stale <= 0 {
		return 30 * time.Second
	}
	return p.Stale
}

func (p Policy) quorum() float64 {
	if p.Quorum <= 0 {
		return 0.5
	}
	return p.Quorum
}

// Floor is how many of n sites must stay up.
func (p Policy) Floor(n int) int {
	f := max(p.MinUp, int(math.Ceil(p.MinFraction*float64(n))))
	return min(max(f, 1), n)
}

// Decision is a mode and why.
type Decision struct {
	Mode   Mode
	Reason string
}

// Input is what a site knows when it decides one service's mode.
type Input struct {
	Service string
	Site    string
	Healthy bool
	// Mode is the site's mode now, and Since when it took it.
	Mode  Mode
	Since time.Time
	// Fleet holds the latest report from every site. Sites, if not nil,
	// is every site that should exist, so one that has never reported
	// counts as unknown rather than being forgotten.
	Fleet []Report
	Sites []string
	Now   time.Time
}

// Decide picks this site's mode for one service.
//
// A site that turns unhealthy degrades first. It withdraws only once
// every fresh report from the other sites is newer than that, so if many
// sites failed in the same moment, each knows it before anyone leaves.
// Without that step they would all see each other's old healthy reports
// and all withdraw together.
//
// Then every site runs the same calculation, so they agree on who
// withdraws without talking it over: the unhealthy sites are ranked,
// those already withdrawn first so decisions stick, then by a hash of
// site and service so the same site isn't always the one held up. Only
// the first few, as many as the floor leaves room for, withdraw.
func Decide(in Input, p Policy) Decision {
	if in.Healthy {
		return Decision{Announce, "healthy"}
	}
	if p.Unguarded {
		return Decision{Withdraw, "unhealthy"}
	}
	if in.Mode == Announce {
		return Decision{Degrade, "unhealthy; degrading first so the other sites see it"}
	}

	all := map[string]*Report{}
	for _, s := range in.Sites {
		all[s] = nil
	}
	for i := range in.Fleet {
		r := &in.Fleet[i]
		all[r.Site] = r
	}
	all[in.Site] = &Report{Site: in.Site, Mode: in.Mode, At: in.Now}

	// Sites we can't vouch for count as unhealthy and holding their routes:
	// they take a withdraw slot, so the floor holds even if they did go.
	var known, others int
	var sick []string
	waiting := false
	for s, r := range all {
		fresh := r != nil && in.Now.Sub(r.At) <= p.stale()
		if s != in.Site {
			others++
			if fresh {
				known++
				waiting = waiting || r.At.Before(in.Since)
			}
		}
		if !fresh || !r.Healthy {
			sick = append(sick, s)
		}
	}
	if others > 0 && float64(known) < p.quorum()*float64(others) {
		// Fail static: keep what we have. A site that is already out, or
		// starting up, stays out rather than announcing unchecked.
		if in.Mode == Withdraw {
			return Decision{Withdraw, "unhealthy, and can't see enough of the fleet to come back"}
		}
		return Decision{Degrade, "can't see enough of the fleet to withdraw safely"}
	}
	if waiting && in.Mode == Degrade {
		return Decision{Degrade, "unhealthy; waiting for the other sites' next reports"}
	}

	n := len(all)
	room := n - p.Floor(n)
	out := func(s string) int {
		if r := all[s]; r != nil && r.Mode == Withdraw {
			return 0
		}
		return 1
	}
	slices.SortFunc(sick, func(a, b string) int {
		return cmp.Or(cmp.Compare(out(a), out(b)), cmp.Compare(rank(in.Service, a), rank(in.Service, b)), cmp.Compare(a, b))
	})
	if i := slices.Index(sick, in.Site); i < room {
		return Decision{Withdraw, "unhealthy"}
	}
	return Decision{Degrade, "unhealthy, but withdrawing would leave too few sites"}
}

func rank(service, site string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(service))
	h.Write([]byte{0})
	h.Write([]byte(site))
	return h.Sum64()
}
