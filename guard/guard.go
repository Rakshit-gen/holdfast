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

// Decide picks this site's mode for one service. fleet holds the latest
// report from every site, this one included; sites, if not nil, is every
// site that should exist, so one that has never reported counts as
// unknown rather than being forgotten.
//
// Every site runs the same calculation, so they agree on who withdraws
// without talking it over: the unhealthy sites are ranked, those already
// withdrawn first so decisions stick, then by a hash of site and service
// so the same site isn't always the one held up. Only the first few, as
// many as the floor leaves room for, withdraw. Two sites deciding on
// different views can both withdraw for a moment; the next round, with
// both reports in, puts one of them back.
func Decide(service, self string, healthy bool, fleet []Report, sites []string, p Policy, now time.Time) Decision {
	if healthy {
		return Decision{Announce, "healthy"}
	}
	if p.Unguarded {
		return Decision{Withdraw, "unhealthy"}
	}

	all := map[string]*Report{}
	for _, s := range sites {
		all[s] = nil
	}
	for i := range fleet {
		r := &fleet[i]
		all[r.Site] = r
	}
	mine := Report{Site: self, At: now}
	if r := all[self]; r != nil {
		mine.Mode = r.Mode
	}
	all[self] = &mine

	// Sites we can't vouch for count as unhealthy and holding their routes:
	// they take a withdraw slot, so the floor holds even if they did go.
	var known, others int
	var sick []string
	for s, r := range all {
		fresh := r != nil && now.Sub(r.At) <= p.stale()
		if s != self {
			others++
			if fresh {
				known++
			}
		}
		if !fresh || !r.Healthy {
			sick = append(sick, s)
		}
	}
	if others > 0 && float64(known) < p.quorum()*float64(others) {
		return Decision{Degrade, "can't see enough of the fleet to withdraw safely"}
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
		return cmp.Or(cmp.Compare(out(a), out(b)), cmp.Compare(rank(service, a), rank(service, b)), cmp.Compare(a, b))
	})
	if i := slices.Index(sick, self); i < room {
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
