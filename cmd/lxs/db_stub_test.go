//go:build !pebble

package main

import (
	"strings"
	"testing"
)

// A binary built without a disk backend, asked to persist, must return a loud
// error naming the fix — never a silent in-memory chain that loses everything on
// restart. Pins that the message points at the right build command and path.
func TestOpenDBWithoutBackendFailsLoud(t *testing.T) {
	_, err := openDB("/some/datadir")
	if err == nil {
		t.Fatal("openDB without the pebble tag must ERROR, not silently run in memory")
	}
	msg := err.Error()
	for _, want := range []string{"-tags pebble", "./cmd/lxs", "/some/datadir", "in memory"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("fail-loud message missing %q:\n%s", want, msg)
		}
	}
}
