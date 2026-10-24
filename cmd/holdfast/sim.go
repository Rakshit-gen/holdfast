package main

import (
	"flag"
	"fmt"
	"os"
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
