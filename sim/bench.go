package sim

import (
	"fmt"
	"io"
	"runtime"
	"sync"
)

// Scenario is a named world.
type Scenario struct {
	Name string
	Note string
	Cfg  Config
}

// Scenarios are the worlds the README compares, each a week of 20 sites
// with the guard's floor at 30%.
func Scenarios() []Scenario {
	quiet := Default()
	quiet.FalseRate, quiet.RegionRate, quiet.PartitionRate = 0, 0, 0

	falseAlarm := quiet
	falseAlarm.FalseRate = 0.3

	region := quiet
	region.RegionRate = 0.3

	bigRegion := region
	bigRegion.RegionShare = 0.8

	part := quiet
	part.PartitionRate = 1

	return []Scenario{
		{"site failures", "each site fails for real 0.2 times a day, 20 minutes on average", quiet},
		{"false alarms", "plus every check failing at once 0.3 times a day, 30 minutes, while the service works", falseAlarm},
		{"regional", "plus 30% of sites failing for real together 0.3 times a day, 45 minutes", region},
		{"big regional", "the same, but 80% of sites at once", bigRegion},
		{"partitions", "plus each site losing sight of the fleet once a day for 15 minutes", part},
		{"mixed", "site failures, false alarms, 30% regional outages and partitions at lower rates", Default()},
	}
}

// Mean runs every policy over seeds 1..n and averages.
func Mean(c Config, n int) []Result {
	out := make([]Result, len(Policies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for seed := 1; seed <= n; seed++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			cc := c
			cc.Seed = uint64(seed)
			w := newWorld(cc)
			for i, p := range Policies {
				r := w.run(p)
				mu.Lock()
				out[i].Policy = p
				out[i].Failed += r.Failed / float64(n)
				out[i].Dark += r.Dark
				out[i].Withdrawals += r.Withdrawals
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for i := range out {
		out[i].Dark /= n
		out[i].Withdrawals /= n
	}
	return out
}

// Bench writes the comparison as markdown.
func Bench(w io.Writer, seeds int) {
	fmt.Fprintf(w, "Invented world: %d sites for a week, %d seeds per row, guard floor %.0f%% of sites. Numbers are means per week.\n\n",
		Default().Sites, seeds, 100*Default().Floor)
	fmt.Fprintf(w, "| scenario | static: failed | unguarded: failed | guarded: failed | unguarded: dark minutes | guarded: dark minutes |\n|---|---|---|---|---|---|\n")
	for _, s := range Scenarios() {
		r := Mean(s.Cfg, seeds)
		fmt.Fprintf(w, "| %s | %s | %s | %s | %d | %d |\n", s.Name, pct(r[0].Failed), pct(r[1].Failed), pct(r[2].Failed), r[1].Dark, r[2].Dark)
	}
	fmt.Fprintf(w, "\nScenarios:\n\n")
	for _, s := range Scenarios() {
		fmt.Fprintf(w, "- **%s:** %s.\n", s.Name, s.Note)
	}

	// The floor costs most when a large share of sites is broken for real:
	// the sick ones it holds up keep some of their users.
	fmt.Fprintf(w, "\n### What the floor costs, big regional world\n\n| floor | failed | dark minutes |\n|---|---|---|\n")
	for _, f := range []float64{0.05, 0.1, 0.2, 0.3, 0.5, 0.7} {
		c := Scenarios()[3].Cfg
		c.Floor = f
		r := Mean(c, seeds)
		fmt.Fprintf(w, "| %.0f%% | %s | %d |\n", 100*f, pct(r[2].Failed), r[2].Dark)
	}
}

func pct(f float64) string { return fmt.Sprintf("%.3f%%", 100*f) }
