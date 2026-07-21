package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Empty dataDir means no persistence: the key is ephemeral (generated, saved
// nowhere). The bool is load-bearing — cmd/lxs warns that a faucet/coinbase/seed
// key is lost on restart based on it.
func TestLoadOrCreateKeyEphemeralWhenNoDataDir(t *testing.T) {
	k, ephemeral, err := loadOrCreateKey("", "faucet.key")
	if err != nil {
		t.Fatal(err)
	}
	if !ephemeral {
		t.Fatal("no dataDir must yield an EPHEMERAL key (nothing to persist to)")
	}
	if k == nil {
		t.Fatal("key is nil")
	}
}

// With a dataDir the key is created once and reloaded identically on restart; a
// coinbase/faucet wallet that changed address every boot would strand its funds.
func TestLoadOrCreateKeyPersistsAndReloadsSameKey(t *testing.T) {
	dir := t.TempDir()

	k1, ephemeral, err := loadOrCreateKey(dir, "faucet.key")
	if err != nil {
		t.Fatal(err)
	}
	if ephemeral {
		t.Fatal("a persisted key must NOT report ephemeral")
	}
	path := filepath.Join(dir, "faucet.key")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file was not written: %v", err)
	}
	// A private key on disk must not be world-readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perms = %o, want 600", perm)
	}

	k2, ephemeral2, err := loadOrCreateKey(dir, "faucet.key")
	if err != nil {
		t.Fatal(err)
	}
	if ephemeral2 {
		t.Fatal("reload must not report ephemeral")
	}
	if k1.Address() != k2.Address() {
		t.Fatalf("reload produced a DIFFERENT key: %s vs %s", k1.Address().Hex(), k2.Address().Hex())
	}
}

// A corrupt key file must fail loudly, never silently mint a new identity that
// abandons the funded wallet.
func TestLoadOrCreateKeyRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "faucet.key"), []byte("not-a-hex-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateKey(dir, "faucet.key"); err == nil {
		t.Fatal("a corrupt key file must be a loud error, not a silent new key")
	}
}

// diskFree must never panic or report free > total; on unix it reports real,
// non-zero space. Used by the health monitor's disk self-diagnosis.
func TestDiskFreeIsSane(t *testing.T) {
	free, total := diskFree(t.TempDir())
	if free > total {
		t.Fatalf("diskFree reported free (%d) > total (%d)", free, total)
	}
}

// Token name/symbol must be printable ASCII within length caps — a launchpad
// anti-phishing check (Avalanche enforces the same).
func TestValidateTokenMeta(t *testing.T) {
	if err := validateTokenMeta("My Coin", "MYC"); err != nil {
		t.Fatalf("a normal name/symbol was rejected: %v", err)
	}
	bad := []struct{ name, sym string }{
		{"", "MYC"},                  // empty name
		{"Coin", ""},                 // empty symbol
		{"Coin", "THISISTOOLONGSYM"}, // symbol too long
		{"bad\x01ctrl", "MYC"},       // control char
		{" leading", "MYC"},          // leading whitespace
		{"trailing ", "MYC"},         // trailing whitespace
	}
	for _, c := range bad {
		if err := validateTokenMeta(c.name, c.sym); err == nil {
			t.Fatalf("bad meta accepted: name=%q symbol=%q", c.name, c.sym)
		}
	}
}
