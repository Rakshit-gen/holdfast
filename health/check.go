// Package health runs the checks that decide whether a site should
// announce a service's prefixes: TCP, HTTP, DNS, a command, or a drain
// file, each repeated with rise and fall thresholds and flap damping.
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Check is one probe. It returns nil when the service looks healthy.
type Check interface {
	Check(ctx context.Context) error
	String() string
}

// TCP passes when a connection to Addr opens.
type TCP struct{ Addr string }

func (c TCP) String() string { return "tcp " + c.Addr }

// Check dials and closes.
func (c TCP) Check(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// HTTP passes when a GET returns a status in [200, 400), or exactly
// Status if set, and the body contains Contains if set.
type HTTP struct {
	URL      string
	Status   int
	Contains string
	Host     string // sent as the Host header, for checking one vhost on a local address
	Client   *http.Client
}

func (c HTTP) String() string { return "http " + c.URL }

// Check does the GET.
func (c HTTP) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}
	if c.Host != "" {
		req.Host = c.Host
	}
	req.Header.Set("User-Agent", "holdfast-health")
	cl := c.Client
	if cl == nil {
		cl = noRedirects
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if c.Status != 0 && resp.StatusCode != c.Status {
		return fmt.Errorf("status %d, want %d", resp.StatusCode, c.Status)
	}
	if c.Status == 0 && (resp.StatusCode < 200 || resp.StatusCode >= 400) {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if c.Contains != "" {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), c.Contains) {
			return fmt.Errorf("body doesn't contain %q", c.Contains)
		}
	}
	return nil
}

// A health check should see the redirect itself, not follow it to
// somewhere else that happens to be up.
var noRedirects = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{DisableKeepAlives: true},
}

// DNS passes when Server answers a lookup of Name with at least one
// address. This is the check an anycast DNS site wants: is the local
// resolver or authoritative server answering, not just listening.
type DNS struct {
	Server string // host:port
	Name   string
}

func (c DNS) String() string { return "dns " + c.Name + " @" + c.Server }

// Check sends the query to Server over UDP.
func (c DNS) Check(ctx context.Context) error {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", c.Server)
		},
	}
	addrs, err := r.LookupHost(ctx, c.Name)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return errors.New("no addresses")
	}
	return nil
}

// Exec passes when the command exits 0. It runs without a shell.
type Exec struct{ Argv []string }

func (c Exec) String() string { return "exec " + strings.Join(c.Argv, " ") }

// Check runs the command, and kills it if ctx ends first.
func (c Exec) Check(ctx context.Context) error {
	if len(c.Argv) == 0 {
		return errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// Absent passes while Path doesn't exist. Touching the file drains the
// site by hand: `touch /etc/holdfast/drain` before maintenance.
type Absent struct{ Path string }

func (c Absent) String() string { return "absent " + c.Path }

// Check stats the file.
func (c Absent) Check(context.Context) error {
	_, err := os.Stat(c.Path)
	if err == nil {
		return fmt.Errorf("%s exists", c.Path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// All passes when every check passes, run in order. The error names the
// first that failed.
type All []Check

func (a All) String() string {
	var ss []string
	for _, c := range a {
		ss = append(ss, c.String())
	}
	return strings.Join(ss, ", ")
}

// Check runs each check until one fails.
func (a All) Check(ctx context.Context) error {
	for _, c := range a {
		if err := c.Check(ctx); err != nil {
			return fmt.Errorf("%s: %w", c, err)
		}
	}
	return nil
}
