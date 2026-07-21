package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lxs/crypto"
)

// TestResolveMinerCoinbasePastedAddress: a user who pastes a valid address mines to it,
// and the address is saved so the next run doesn't ask again.
func TestResolveMinerCoinbasePastedAddress(t *testing.T) {
	dir := t.TempDir()
	k, _ := crypto.GenerateKey()
	pasted := k.Address().Hex()

	got, err := resolveMinerCoinbase(dir, strings.NewReader(pasted+"\n"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.EqualFold(got, pasted) {
		t.Fatalf("coinbase = %s, want %s", got, pasted)
	}
	if saved, ok := readSavedAddress(filepath.Join(dir, "lxs-wallet.txt")); !ok || !strings.EqualFold(saved, pasted) {
		t.Fatalf("pasted address was not saved for reuse (ok=%v saved=%s)", ok, saved)
	}
}

// TestResolveMinerCoinbaseCreatesWallet: pressing Enter (empty input) mints a fresh
// wallet, returns a usable address, and stores its private key so the coins are
// recoverable.
func TestResolveMinerCoinbaseCreatesWallet(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveMinerCoinbase(dir, strings.NewReader("\n"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 42 || !strings.HasPrefix(got, "0x") {
		t.Fatalf("created address looks wrong: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "lxs-wallet.txt"))
	if err != nil {
		t.Fatalf("wallet file not written: %v", err)
	}
	if !strings.Contains(string(data), "private key") {
		t.Fatal("a created wallet MUST save its private key, else the mined coins are unrecoverable")
	}
	if saved, ok := readSavedAddress(filepath.Join(dir, "lxs-wallet.txt")); !ok || !strings.EqualFold(saved, got) {
		t.Fatalf("created address not readable back (ok=%v saved=%s)", ok, saved)
	}
}

// TestResolveMinerCoinbaseReusesSaved: a second run reuses the saved wallet WITHOUT
// touching input — proven by handing it a reader that fails the test if read.
func TestResolveMinerCoinbaseReusesSaved(t *testing.T) {
	dir := t.TempDir()
	first, err := resolveMinerCoinbase(dir, strings.NewReader("\n")) // create
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := resolveMinerCoinbase(dir, failingReader{t})
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if !strings.EqualFold(first, second) {
		t.Fatalf("second run did not reuse the saved address: %s vs %s", first, second)
	}
}

// TestResolveMinerCoinbaseRejectsGarbage: a non-address paste is refused, not silently
// mined to a bad coinbase.
func TestResolveMinerCoinbaseRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if _, err := resolveMinerCoinbase(dir, strings.NewReader("not-an-address\n")); err == nil {
		t.Fatal("garbage address should be rejected")
	}
}

// TestResolveMinerCoinbaseCreatesDataDir: the datadir often does not exist yet on the
// very first run (the packaged miner points -datadir at a "data" subfolder). Saving the
// wallet must create it, not fail.
func TestResolveMinerCoinbaseCreatesDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist", "data")
	got, err := resolveMinerCoinbase(dir, strings.NewReader("\n"))
	if err != nil {
		t.Fatalf("should create a missing datadir, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lxs-wallet.txt")); err != nil {
		t.Fatalf("wallet not saved into the created datadir: %v", err)
	}
	if len(got) != 42 {
		t.Fatalf("bad address: %q", got)
	}
}

type failingReader struct{ t *testing.T }

func (f failingReader) Read([]byte) (int, error) {
	f.t.Fatal("resolveMinerCoinbase read input when it should have reused the saved wallet")
	return 0, nil
}
