package apply

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = e.clock.Add(d)
}

// TestForceToken covers plan 3.4 at the token level: valid once, then
// reused; expired; and bound to the diff and the live placement.
func TestForceToken(t *testing.T) {
	e := newEnv(t)
	b := forceBinding{Diff: "d", Bases: []string{"a", ""}, Units: []string{"vtllibrary@10.service"}, R1: "r"}
	refusedWith := func(name string, err error) {
		t.Helper()
		if r, ok := err.(*refusal); !ok || r.code != ErrForce {
			t.Errorf("%s: %v", name, err)
		}
	}

	tok, exp, err := e.h.mintForce(b)
	if err != nil {
		t.Fatal(err)
	}
	if d := exp.Sub(e.clock); d < forceTTL-2*time.Second || d > forceTTL {
		t.Errorf("expires in %v", d)
	}
	if err := e.h.checkForce(tok, b); err != nil {
		t.Fatalf("valid: %v", err)
	}
	refusedWith("reused", e.h.checkForce(tok, b))

	tok, _, _ = e.h.mintForce(b)
	e.advance(forceTTL)
	refusedWith("expired", e.h.checkForce(tok, b))

	tok, _, _ = e.h.mintForce(b)
	diff := b
	diff.Diff = "d2"
	refusedWith("diff changed", e.h.checkForce(tok, diff))
	live := b
	live.R1 = "r2"
	refusedWith("live state changed", e.h.checkForce(tok, live))
	bases := b
	bases.Bases = []string{"a", "x"}
	refusedWith("base changed", e.h.checkForce(tok, bases))
	refusedWith("malformed", e.h.checkForce("x", b))

	// The key is 0600 and kept; expired nonces are pruned.
	fi, err := os.Stat(filepath.Join(e.run, forceKeyFile))
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != 32 {
		t.Errorf("force.key: %v %v", fi, err)
	}
	e.advance(2 * forceTTL)
	tok, _, _ = e.h.mintForce(b)
	if err := e.h.checkForce(tok, b); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(e.run, nonceDir)); len(ents) != 1 {
		t.Errorf("nonces not pruned: %d left", len(ents))
	}
}
