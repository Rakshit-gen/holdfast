package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/Rakshit-gen/holdfast/sim"
)

func simCmd(args []string) error {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	c := sim.Default()
	fs.IntVar(&c.Sites, "sites", c.Sites, "sites in the fleet")
	days := fs.Int("days", c.Minutes/1440, "days to simulate")
	seeds := fs.Int("seeds", 5, "worlds to average over")
	fs.Float64Var(&c.Floor, "floor", c.Floor, "share of sites the guard keeps announcing")
	fs.Float64Var(&c.Leak, "leak", c.Leak, "share of a degraded site's users that still reach it")
	fs.Float64Var(&c.SiteFailRate, "site-fail", c.SiteFailRate, "real failures per site per day")
	fs.Float64Var(&c.FalseRate, "false-alarm", c.FalseRate, "fleet-wide false alarms per day")
	fs.Float64Var(&c.RegionRate, "regional", c.RegionRate, "regional failures per day")
	fs.Float64Var(&c.RegionShare, "region-share", c.RegionShare, "share of sites a regional failure takes")
	fs.Float64Var(&c.PartitionRate, "partition", c.PartitionRate, "partitions per site per day")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.Sites < 1 || *days < 1 || *seeds < 1 {
		return fmt.Errorf("sites, days and seeds must be at least 1")
	}
	c.Minutes = *days * 1440
	fmt.Printf("Invented world: %d sites, %d days, mean of %d seeds.\n\n", c.Sites, *days, *seeds)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POLICY\tFAILED\tDARK MINUTES\tWITHDRAWALS")
	for _, r := range sim.Mean(c, *seeds) {
		fmt.Fprintf(tw, "%s\t%.3f%%\t%d\t%d\n", r.Policy, 100*r.Failed, r.Dark, r.Withdrawals)
	}
	return tw.Flush()
}

func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	seeds := fs.Int("seeds", 5, "worlds to average over per row")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *seeds < 1 {
		return fmt.Errorf("seeds must be at least 1")
	}
	sim.Bench(os.Stdout, *seeds)
	return nil
}

func replayCmd(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	c := sim.Config{Sites: 10, Minutes: 25, Floor: 0.3, Leak: sim.Default().Leak}
	fs.IntVar(&c.Sites, "sites", c.Sites, "sites in the fleet")
	fs.Float64Var(&c.Floor, "floor", c.Floor, "share of sites the guard keeps announcing")
	share := fs.Float64("share", 0, "share of sites that really break; 0 means a false alarm at every site")
	length := fs.Int("minutes", 10, "how long the outage lasts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.Sites < 1 || *length < 1 || *share < 0 || *share > 1 {
		return fmt.Errorf("sites and minutes must be at least 1, share 0 to 1")
	}
	o := sim.Outage{Start: 5, Minutes: *length, False: *share == 0, Share: *share}
	c.Minutes = o.Start + o.Minutes + 5
	c.Script = []sim.Outage{o}
	what := "every check fails while the service works"
	if !o.False {
		what = fmt.Sprintf("%.0f%% of sites really break", 100*o.Share)
	}
	fmt.Printf("Invented world: %d sites. From minute %d to %d, %s.\nEach column is announcing/degraded/withdrawn sites, then failed traffic.\n\n",
		c.Sites, o.Start, o.Start+o.Minutes, what)
	var lines [][]sim.Minute
	for _, p := range sim.Policies {
		lines = append(lines, sim.Timeline(c, p))
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "MINUTE")
	for _, p := range sim.Policies {
		fmt.Fprintf(tw, "\t%s", strings.ToUpper(p.String()))
	}
	fmt.Fprintln(tw)
	for m := range c.Minutes {
		fmt.Fprintf(tw, "%d", m)
		for _, l := range lines {
			x := l[m]
			fmt.Fprintf(tw, "\t%d/%d/%d %3.0f%%", x.Announce, x.Degrade, x.Withdraw, 100*x.Failed)
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}
