package sim

import "testing"

func short() Config {
	c := Default()
	c.Minutes = 2 * 24 * 60
	c.Seed = 7
	return c
}

func TestSameSeedSameResult(t *testing.T) {
	c := short()
	if a, b := Run(c, Guarded), Run(c, Guarded); a != b {
		t.Fatalf("%+v != %+v", a, b)
	}
}

func TestStaticNeverWithdraws(t *testing.T) {
	r := Run(short(), Static)
	if r.Withdrawals != 0 || r.Dark != 0 {
		t.Fatalf("%+v", r)
	}
}

// A false alarm everywhere takes every unguarded site down, and none of
// the guarded ones below the floor.
func TestFalseAlarm(t *testing.T) {
	c := short()
	c.SiteFailRate, c.RegionRate, c.PartitionRate = 0, 0, 0
	c.FalseRate = 2
	if r := Run(c, Unguarded); r.Dark == 0 {
		t.Fatalf("unguarded never went dark: %+v", r)
	}
	if r := Run(c, Guarded); r.Dark != 0 || r.Failed != 0 {
		t.Fatalf("guarded: %+v", r)
	}
}

func TestHealthyWorldFailsNothing(t *testing.T) {
	c := short()
	c.SiteFailRate, c.FalseRate, c.RegionRate, c.PartitionRate = 0, 0, 0, 0
	for _, p := range Policies {
		if r := Run(c, p); r.Failed != 0 || r.Withdrawals != 0 {
			t.Errorf("%v: %+v", p, r)
		}
	}
}

func TestGuardBeatsStaticOnRealFailures(t *testing.T) {
	c := short()
	if s, g := Run(c, Static), Run(c, Guarded); g.Failed >= s.Failed {
		t.Fatalf("guarded %v, static %v", g.Failed, s.Failed)
	}
}
