package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// stdout runs f and returns what it printed.
func stdout(t *testing.T, f func() error) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := f()
	os.Stdout = old
	w.Close()
	b, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSimCommand(t *testing.T) {
	out := stdout(t, func() error { return simCmd([]string{"-days", "1", "-seeds", "1", "-false-alarm", "3"}) })
	for _, want := range []string{"Invented world", "static", "unguarded", "guarded"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if err := simCmd([]string{"-sites", "0"}); err == nil {
		t.Fatal("0 sites accepted")
	}
}

func TestReplayCommand(t *testing.T) {
	out := stdout(t, func() error { return replayCmd([]string{"-minutes", "3"}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Two header lines, a blank, the column names, and 5+3+5 minutes.
	if len(lines) != 4+13 {
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
	// Minute 6, a false alarm under way: unguarded has nothing left.
	if !strings.Contains(lines[4+6], "0/0/10 100%") {
		t.Fatalf("minute 6: %q", lines[4+6])
	}
}
