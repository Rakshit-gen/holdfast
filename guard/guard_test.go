package guard

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func fleetOf(n int, sick ...int) ([]Report, []string) {
	var rs []Report
	var names []string
	bad := map[int]bool{}
	for _, i := range sick {
		bad[i] = true
	}
	for i := range n {
		name := fmt.Sprintf("site%02d", i)
		names = append(names, name)
		rs = append(rs, Report{Site: name, Healthy: !bad[i], At: t0})
	}
	return rs, names
}

func TestHealthyAnnounces(t *testing.T) {
	rs, names := fleetOf(5)
	if d := Decide("dns", "site00", true, rs, names, Policy{MinUp: 3}, t0); d.Mode != Announce {
		t.Fatal(d)
	}
}

func TestOneSickSiteWithdraws(t *testing.T) {
	rs, names := fleetOf(10, 3)
	if d := Decide("dns", "site03", false, rs, names, Policy{MinUp: 3}, t0); d.Mode != Withdraw {
		t.Fatal(d)
	}
}

// When every site fails at once, exactly enough withdraw to reach the
// floor, and every site agrees which ones.
func TestEveryoneSickKeepsTheFloor(t *testing.T) {
	all := []int{}
	for i := range 20 {
		all = append(all, i)
	}
	rs, names := fleetOf(20, all...)
	for _, p := range []Policy{{MinUp: 3}, {MinFraction: 0.5}, {MinUp: 2, MinFraction: 0.25}} {
		withdrawn := 0
		for _, s := range names {
			if Decide("dns", s, false, rs, names, p, t0).Mode == Withdraw {
				withdrawn++
			}
		}
		if want := 20 - p.Floor(20); withdrawn != want {
			t.Errorf("%+v: %d withdrew, want %d", p, withdrawn, want)
		}
	}
}

func TestUnguardedWithdrawsEverything(t *testing.T) {
	rs, names := fleetOf(4, 0, 1, 2, 3)
	for _, s := range names {
		if Decide("dns", s, false, rs, names, Policy{MinUp: 2, Unguarded: true}, t0).Mode != Withdraw {
			t.Fatal(s)
		}
	}
}

func TestFailStaticWithoutQuorum(t *testing.T) {
	rs, names := fleetOf(10, 0)
	// Our view of 6 of the 9 others is a minute old.
	for i := 1; i <= 6; i++ {
		rs[i].At = t0.Add(-time.Minute)
	}
	d := Decide("dns", "site00", false, rs, names, Policy{MinUp: 1}, t0)
	if d.Mode != Degrade {
		t.Fatal(d)
	}
	// A site that never reported at all counts the same.
	d = Decide("dns", "site00", false, rs[:1], names, Policy{MinUp: 1}, t0)
	if d.Mode != Degrade {
		t.Fatal(d)
	}
}

func TestUnknownSitesTakeWithdrawSlots(t *testing.T) {
	// 5 sites, floor 3, so 2 may withdraw. One other site is silent: it
	// may already be gone, so only one slot is left for the 2 sick ones.
	rs, names := fleetOf(5, 0, 1)
	rs[4].At = t0.Add(-time.Hour)
	withdrawn := 0
	for _, s := range names[:2] {
		if Decide("dns", s, false, rs, names, Policy{MinUp: 3}, t0).Mode == Withdraw {
			withdrawn++
		}
	}
	if withdrawn != 1 {
		t.Fatalf("%d withdrew", withdrawn)
	}
}

func TestWithdrawnSitesKeepTheirSlot(t *testing.T) {
	// Room for 1. Whichever site already withdrew stays withdrawn, so a
	// newly sick site doesn't make the two swap.
	rs, names := fleetOf(4, 0, 1)
	for _, first := range []int{0, 1} {
		rs[0].Mode, rs[1].Mode = Announce, Announce
		rs[first].Mode = Withdraw
		for i, s := range names[:2] {
			d := Decide("dns", s, false, rs, names, Policy{MinUp: 3}, t0)
			if (d.Mode == Withdraw) != (i == first) {
				t.Errorf("first=%d: %s got %v", first, s, d)
			}
		}
	}
}

func TestRankSpreadsAcrossServices(t *testing.T) {
	// With room for one, which site withdraws should depend on the
	// service, so one site isn't always the one held up.
	rs, names := fleetOf(8, 0, 1, 2, 3, 4, 5, 6, 7)
	picked := map[string]bool{}
	for i := range 20 {
		svc := fmt.Sprintf("svc%d", i)
		for _, s := range names {
			if Decide(svc, s, false, rs, names, Policy{MinUp: 7}, t0).Mode == Withdraw {
				picked[s] = true
			}
		}
	}
	if len(picked) < 4 {
		t.Fatalf("only %v were ever picked", picked)
	}
}

func TestFloor(t *testing.T) {
	cases := []struct {
		p    Policy
		n    int
		want int
	}{
		{Policy{}, 10, 1},
		{Policy{MinUp: 3}, 10, 3},
		{Policy{MinUp: 3}, 2, 2},
		{Policy{MinFraction: 0.34}, 10, 4},
		{Policy{MinUp: 5, MinFraction: 0.1}, 10, 5},
	}
	for _, c := range cases {
		if got := c.p.Floor(c.n); got != c.want {
			t.Errorf("%+v of %d: %d, want %d", c.p, c.n, got, c.want)
		}
	}
}
