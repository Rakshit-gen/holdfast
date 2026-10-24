// Command holdfast announces a site's anycast prefixes over BGP while
// their health checks pass, and keeps enough sites announcing that a
// fleet-wide false alarm can't take the whole service off the internet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Rakshit-gen/holdfast/config"
	"github.com/Rakshit-gen/holdfast/daemon"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const (
	defaultConfig = "/etc/holdfast/holdfast.json"
	defaultAPI    = "127.0.0.1:9179"
)

const usage = `holdfast announces anycast prefixes while they're healthy, and never
lets the whole fleet withdraw at once.

  holdfast run     [-config /etc/holdfast/holdfast.json]
  holdfast check   [-config ...] [-probe]
  holdfast status  [-api 127.0.0.1:9179] [-json] [-v]
  holdfast drain   [-api ...] [service]
  holdfast undrain [-api ...] [service]
  holdfast sim     [-floor 0.3] [-false-alarm 0.1] ...
  holdfast bench   [-seeds 5]
  holdfast replay  [-share 0] [-sites 10] [-floor 0.3]
  holdfast version

Run a command with -h for its flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"run": runCmd, "check": checkCmd, "status": statusCmd,
		"drain": drainCmd(true), "undrain": drainCmd(false), "sim": simCmd, "bench": benchCmd, "replay": replayCmd,
	}
	name, args := os.Args[1], os.Args[2:]
	switch name {
	case "version", "--version", "-v":
		fmt.Printf("holdfast %s (%s, %s)\n", version, commit, date)
		return
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	}
	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "holdfast: no command %q\n\n%s", name, usage)
		os.Exit(2)
	}
	if err := cmd(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "holdfast:", err)
		}
		os.Exit(1)
	}
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultConfig, "config file")
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	daemon.Version = version
	logger := log.New(os.Stderr, "", log.LstdFlags)
	d, err := daemon.New(cfg, logger.Printf)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Printf("holdfast %s: site %s, %d services, %d neighbors", version, cfg.Site, len(cfg.Services), len(cfg.Neighbors))
	return d.Run(ctx)
}

func checkCmd(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	path := configFlag(fs)
	probe := fs.Bool("probe", false, "also run every service's checks once")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	sites := len(cfg.Fleet.Peers) + 1
	fmt.Printf("%s: ok. Site %s, %d neighbors, %d sites in the fleet.\n", *path, cfg.Site, len(cfg.Neighbors), sites)
	for _, s := range cfg.Services {
		pol := cfg.Policy(s)
		if pol.Unguarded {
			fmt.Printf("  %s: %d prefixes, unguarded, so every site may withdraw at once\n", s.Name, len(s.Prefixes))
			continue
		}
		note := ""
		if pol.Floor(sites) == sites {
			note = "; no site can ever withdraw, only degrade"
		}
		fmt.Printf("  %s: %d prefixes, at least %d of %d sites keep announcing%s\n",
			s.Name, len(s.Prefixes), pol.Floor(sites), sites, note)
	}
	if !*probe {
		return nil
	}
	failed := 0
	for _, s := range cfg.Services {
		p := cfg.Probe(s)
		timeout := p.Timeout
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := p.Check.Check(ctx)
		cancel()
		if err != nil {
			failed++
			fmt.Printf("  %s: failing: %v\n", s.Name, err)
		} else {
			fmt.Printf("  %s: passing\n", s.Name)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d services failing their checks", failed, len(cfg.Services))
	}
	return nil
}
