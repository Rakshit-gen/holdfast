// Package fleet shares each site's health and announce mode with the
// other sites. Every site serves its own reports over HTTP and polls the
// others. There's no coordinator to lose: a site that can't reach the
// others just sees their reports go stale, and the guard treats that as
// a reason not to withdraw.
package fleet

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Rakshit-gen/holdfast/guard"
)

// Wire is what GET /v1/reports returns.
type Wire struct {
	Site     string        `json:"site"`
	Services []WireService `json:"services"`
}

// WireService is one service's report. Age is how long ago the site
// decided it, by its own clock, so clocks needn't agree.
type WireService struct {
	Service string  `json:"service"`
	Healthy bool    `json:"healthy"`
	Mode    string  `json:"mode"`
	Age     float64 `json:"age_seconds"`
}

// Peer is another site to poll.
type Peer struct {
	Site string
	URL  string // base URL, like http://198.51.100.7:9179
}

// Node is this site's end.
type Node struct {
	Site     string
	Peers    []Peer
	Token    string        // sent and required as a bearer token, if set
	Interval time.Duration // 0 means 5s
	Client   *http.Client
	Logf     func(string, ...any)

	mu     sync.Mutex
	mine   map[string]mineEntry
	theirs map[string]map[string]guard.Report // site, service
	errs   map[string]string                  // site, last poll error
}

type mineEntry struct {
	healthy bool
	mode    guard.Mode
	at      time.Time
}

// Set records this site's state for a service.
func (n *Node) Set(service string, healthy bool, mode guard.Mode) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.mine == nil {
		n.mine = map[string]mineEntry{}
	}
	old, ok := n.mine[service]
	if ok && old.healthy == healthy && old.mode == mode {
		// Unchanged, but it's still current.
		old.at = time.Now()
		n.mine[service] = old
		return
	}
	n.mine[service] = mineEntry{healthy, mode, time.Now()}
}

// Sites lists every site, this one first.
func (n *Node) Sites() []string {
	out := []string{n.Site}
	for _, p := range n.Peers {
		out = append(out, p.Site)
	}
	return out
}

// View returns the latest report from every site that has one for the
// service, this site included.
func (n *Node) View(service string) []guard.Report {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []guard.Report
	if m, ok := n.mine[service]; ok {
		out = append(out, guard.Report{Site: n.Site, Healthy: m.healthy, Mode: m.mode, At: m.at})
	}
	for _, p := range n.Peers {
		if r, ok := n.theirs[p.Site][service]; ok {
			out = append(out, r)
		}
	}
	return out
}

// PeerErrors returns the last poll error for each peer that has one.
func (n *Node) PeerErrors() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]string{}
	for k, v := range n.errs {
		out[k] = v
	}
	return out
}

// ServeHTTP answers GET /v1/reports.
func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if n.Token != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(n.Token)) != 1 {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(n.wire(time.Now()))
}

func (n *Node) wire(now time.Time) Wire {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := Wire{Site: n.Site, Services: []WireService{}}
	for svc, m := range n.mine {
		out.Services = append(out.Services, WireService{svc, m.healthy, m.mode.String(), now.Sub(m.at).Seconds()})
	}
	slices.SortFunc(out.Services, func(a, b WireService) int { return strings.Compare(a.Service, b.Service) })
	return out
}

func (n *Node) defaults() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.Interval <= 0 {
		n.Interval = 5 * time.Second
	}
	if n.Client == nil {
		n.Client = &http.Client{Timeout: n.Interval}
	}
	if n.Logf == nil {
		n.Logf = func(string, ...any) {}
	}
}

// Run polls every peer until ctx ends.
func (n *Node) Run(ctx context.Context) {
	n.defaults()
	var wg sync.WaitGroup
	for _, p := range n.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(n.Interval)
			defer t.Stop()
			for {
				n.poll(ctx, p)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	wg.Wait()
}

func (n *Node) poll(ctx context.Context, p Peer) {
	n.defaults()
	err := n.fetch(ctx, p)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.errs == nil {
		n.errs = map[string]string{}
	}
	if err != nil {
		if n.errs[p.Site] == "" {
			n.Logf("fleet: can't reach %s: %v", p.Site, err)
		}
		n.errs[p.Site] = err.Error()
		return
	}
	if n.errs[p.Site] != "" {
		n.Logf("fleet: reached %s again", p.Site)
	}
	delete(n.errs, p.Site)
}

func (n *Node) fetch(ctx context.Context, p Peer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.URL, "/")+"/v1/reports", nil)
	if err != nil {
		return err
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	start := time.Now()
	resp, err := n.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var w Wire
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&w); err != nil {
		return err
	}
	if w.Site != p.Site {
		return fmt.Errorf("%s answered as %q", p.URL, w.Site)
	}
	got := map[string]guard.Report{}
	for _, s := range w.Services {
		mode, ok := parseMode(s.Mode)
		if !ok || s.Age < 0 {
			return fmt.Errorf("bad report for %s", s.Service)
		}
		// Count age from when we asked, so a slow answer looks older.
		at := start.Add(-time.Duration(s.Age * float64(time.Second)))
		got[s.Service] = guard.Report{Site: p.Site, Healthy: s.Healthy, Mode: mode, At: at}
	}
	n.mu.Lock()
	if n.theirs == nil {
		n.theirs = map[string]map[string]guard.Report{}
	}
	n.theirs[p.Site] = got
	n.mu.Unlock()
	return nil
}

func parseMode(s string) (guard.Mode, bool) {
	for m := guard.Announce; m <= guard.Withdraw; m++ {
		if m.String() == s {
			return m, true
		}
	}
	return 0, false
}
