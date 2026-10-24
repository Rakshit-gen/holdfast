package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Rakshit-gen/holdfast/bgp"
)

// Version is reported in the status; the command sets it at build time.
var Version = "dev"

// Status is what GET /v1/status returns.
type Status struct {
	Site      string            `json:"site"`
	Version   string            `json:"version"`
	Services  []ServiceStatus   `json:"services"`
	Neighbors []NeighborStatus  `json:"neighbors"`
	Fleet     map[string]string `json:"fleet_errors"`
}

// ServiceStatus is one service.
type ServiceStatus struct {
	Name    string    `json:"name"`
	Healthy bool      `json:"healthy"`
	Drained bool      `json:"drained"`
	Mode    string    `json:"mode"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"`
	LastErr string    `json:"last_error,omitempty"`
	Held    int       `json:"held"`
	Holding bool      `json:"holding"` // the guard is keeping this site up now
	Changes int       `json:"changes"`
	// Sites is how each site in the fleet reports this service.
	Sites []SiteStatus `json:"sites"`
}

// SiteStatus is one site's report as this site last saw it.
type SiteStatus struct {
	Site    string  `json:"site"`
	Healthy bool    `json:"healthy"`
	Mode    string  `json:"mode"`
	Age     float64 `json:"age_seconds"`
}

// NeighborStatus is one BGP session.
type NeighborStatus struct {
	Address   string    `json:"address"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	Sent      int       `json:"prefixes_sent"`
	Received  int       `json:"prefixes_received"`
	Flaps     int       `json:"flaps"`
	LastError string    `json:"last_error,omitempty"`
}

// Status gathers the current state.
func (d *Daemon) Status() Status {
	now := time.Now()
	errs := map[string]string{}
	for _, st := range d.prober.Status() {
		errs[st.Name] = st.LastErr
	}
	out := Status{Site: d.cfg.Site, Version: Version, Fleet: d.fleet.PeerErrors()}
	d.mu.Lock()
	for name, s := range d.svc {
		ss := ServiceStatus{
			Name: name, Healthy: s.healthy, Drained: s.drained, Mode: s.mode.String(), Reason: s.reason,
			Since: s.since, Held: s.held, Holding: s.holding, Changes: s.changes, Sites: []SiteStatus{},
		}
		if !s.healthy {
			ss.LastErr = errs[name]
		}
		for _, r := range d.fleet.View(name) {
			ss.Sites = append(ss.Sites, SiteStatus{r.Site, r.Healthy, r.Mode.String(), now.Sub(r.At).Seconds()})
		}
		out.Services = append(out.Services, ss)
	}
	d.mu.Unlock()
	slices.SortFunc(out.Services, func(a, b ServiceStatus) int { return strings.Compare(a.Name, b.Name) })
	for addr, s := range d.sessions {
		st := s.Status()
		out.Neighbors = append(out.Neighbors, NeighborStatus{
			Address: addr.String(), State: st.State.String(), Since: st.Since,
			Sent: st.Sent, Received: st.Received, Flaps: st.Flaps, LastError: st.LastError,
		})
	}
	slices.SortFunc(out.Neighbors, func(a, b NeighborStatus) int { return strings.Compare(a.Address, b.Address) })
	return out
}

// API serves the local control API:
//
//	GET  /v1/status
//	POST /v1/drain?service=NAME     (all services without service=)
//	POST /v1/undrain?service=NAME
//	GET  /metrics
func (d *Daemon) API() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(d.Status())
	})
	drain := func(on bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := d.Drain(r.URL.Query().Get("service"), on); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /v1/drain", drain(true))
	mux.HandleFunc("POST /v1/undrain", drain(false))
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		d.metrics(w)
	})
	return mux
}

// metrics writes the Prometheus text format by hand; it's a dozen lines.
func (d *Daemon) metrics(w io.Writer) {
	st := d.Status()
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	p("# HELP holdfast_service_healthy Whether the service's checks pass at this site.")
	p("# TYPE holdfast_service_healthy gauge")
	for _, s := range st.Services {
		p("holdfast_service_healthy{service=%q} %d", s.Name, b(s.Healthy))
	}
	p("# HELP holdfast_service_mode 0 announce, 1 degrade, 2 withdraw.")
	p("# TYPE holdfast_service_mode gauge")
	for _, s := range st.Services {
		p("holdfast_service_mode{service=%q,mode=%q} %d", s.Name, s.Mode, modeValue(s.Mode))
	}
	p("# HELP holdfast_guard_held_total Times the guard kept this site announcing while unhealthy.")
	p("# TYPE holdfast_guard_held_total counter")
	for _, s := range st.Services {
		p("holdfast_guard_held_total{service=%q} %d", s.Name, s.Held)
	}
	p("# HELP holdfast_service_changes_total Times the service's mode changed at this site.")
	p("# TYPE holdfast_service_changes_total counter")
	for _, s := range st.Services {
		p("holdfast_service_changes_total{service=%q} %d", s.Name, s.Changes)
	}
	p("# HELP holdfast_guard_holding Whether the guard is keeping this site announcing while unhealthy.")
	p("# TYPE holdfast_guard_holding gauge")
	for _, s := range st.Services {
		p("holdfast_guard_holding{service=%q} %d", s.Name, b(s.Holding))
	}
	p("# HELP holdfast_fleet_sites_announcing Sites announcing the service, as this site sees them.")
	p("# TYPE holdfast_fleet_sites_announcing gauge")
	for _, s := range st.Services {
		n := 0
		for _, x := range s.Sites {
			n += b(x.Mode != "withdraw")
		}
		p("holdfast_fleet_sites_announcing{service=%q} %d", s.Name, n)
	}
	p("# HELP holdfast_bgp_established Whether the BGP session is established.")
	p("# TYPE holdfast_bgp_established gauge")
	for _, n := range st.Neighbors {
		p("holdfast_bgp_established{neighbor=%q} %d", n.Address, b(n.State == bgp.Established.String()))
	}
	p("# HELP holdfast_bgp_prefixes_sent Prefixes the neighbor has from this site.")
	p("# TYPE holdfast_bgp_prefixes_sent gauge")
	for _, n := range st.Neighbors {
		p("holdfast_bgp_prefixes_sent{neighbor=%q} %d", n.Address, n.Sent)
	}
	p("# HELP holdfast_bgp_flaps_total Times the session left established.")
	p("# TYPE holdfast_bgp_flaps_total counter")
	for _, n := range st.Neighbors {
		p("holdfast_bgp_flaps_total{neighbor=%q} %d", n.Address, n.Flaps)
	}
	p("# HELP holdfast_fleet_peer_reachable Whether the last poll of the peer site worked.")
	p("# TYPE holdfast_fleet_peer_reachable gauge")
	for _, peer := range d.cfg.Fleet.Peers {
		_, bad := st.Fleet[peer.Site]
		p("holdfast_fleet_peer_reachable{site=%q} %d", peer.Site, b(!bad))
	}
}

func modeValue(m string) int {
	switch m {
	case "degrade":
		return 1
	case "withdraw":
		return 2
	}
	return 0
}
