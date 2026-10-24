package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Rakshit-gen/holdfast/daemon"
)

var client = &http.Client{Timeout: 5 * time.Second}

func apiFlag(fs *flag.FlagSet) *string {
	return fs.String("api", defaultAPI, "address of a running holdfast's API")
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	api := apiFlag(fs)
	raw := fs.Bool("json", false, "print the JSON as the API returns it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resp, err := client.Get("http://" + *api + "/v1/status")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status: %s", resp.Status)
	}
	if *raw {
		_, err := io.Copy(os.Stdout, resp.Body)
		return err
	}
	var st daemon.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return err
	}
	printStatus(os.Stdout, st, time.Now())
	return nil
}

func printStatus(w io.Writer, st daemon.Status, now time.Time) {
	fmt.Fprintf(w, "site %s\n\n", st.Site)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tHEALTHY\tMODE\tFOR\tFLEET\tREASON")
	for _, s := range st.Services {
		healthy := yes(s.Healthy)
		if s.Drained {
			healthy = "drained"
		}
		var up int
		for _, x := range s.Sites {
			if x.Mode != "withdraw" {
				up++
			}
		}
		mode := s.Mode
		if s.Holding {
			mode += " (held)"
		}
		reason := s.Reason
		if s.LastErr != "" {
			reason += ": " + s.LastErr
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d announcing\t%s\n",
			s.Name, healthy, mode, ago(now, s.Since), up, len(s.Sites), reason)
	}
	tw.Flush()

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NEIGHBOR\tSTATE\tFOR\tSENT\tRECEIVED\tFLAPS\tLAST ERROR")
	for _, n := range st.Neighbors {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			n.Address, n.State, ago(now, n.Since), n.Sent, n.Received, n.Flaps, n.LastError)
	}
	tw.Flush()

	if len(st.Fleet) > 0 {
		fmt.Fprintln(w, "\nsites this site can't reach:")
		for _, site := range slices.Sorted(maps.Keys(st.Fleet)) {
			fmt.Fprintf(w, "  %s: %s\n", site, st.Fleet[site])
		}
	}
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return now.Sub(t).Round(time.Second).String()
}

func drainCmd(on bool) func([]string) error {
	name := "drain"
	if !on {
		name = "undrain"
	}
	return func(args []string) error {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		api := apiFlag(fs)
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() > 1 {
			return fmt.Errorf("%s takes at most one service", name)
		}
		u := "http://" + *api + "/v1/" + name
		if fs.NArg() == 1 {
			u += "?service=" + url.QueryEscape(fs.Arg(0))
		}
		resp, err := client.Post(u, "", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		what := "every service"
		if fs.NArg() == 1 {
			what = fs.Arg(0)
		}
		fmt.Printf("%sed %s. The guard still decides whether this site may withdraw; see holdfast status.\n", name, what)
		return nil
	}
}
