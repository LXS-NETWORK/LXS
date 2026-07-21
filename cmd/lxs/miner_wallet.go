package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"lxs/common"
	"lxs/crypto"
)

// resolveMinerCoinbase decides which address a one-click miner pays rewards to, with
// zero prior setup. In order: reuse a wallet saved by a previous run; else ask the user
// to paste an address; else create a brand-new wallet on the spot. This is what lets a
// non-technical user just launch the miner and start earning — paste your address, or
// press Enter and we make you one.
//
// in is where the user's answer is read from (os.Stdin in production, a fixed reader in
// tests). dataDir is where the wallet file lives so the same address is reused each run.
func resolveMinerCoinbase(dataDir string, in io.Reader) (string, error) {
	dir := dataDir
	if dir == "" {
		dir = "."
	}
	// The datadir may not exist yet (the node creates it later, but we save the wallet
	// here first), so make sure it's there before writing into it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating data directory %q: %w", dir, err)
	}
	walletPath := filepath.Join(dir, "lxs-wallet.txt")

	// A wallet from a previous run: mine to the same address without asking again.
	if addr, ok := readSavedAddress(walletPath); ok {
		fmt.Printf("Using your saved wallet address: %s\n", addr)
		return addr, nil
	}

	fmt.Println("Paste your LXS address to receive mining rewards,")
	fmt.Print("or just press Enter to create a new wallet for you: ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.TrimSpace(line)

	if line != "" {
		a, err := common.AddressFromHex(line)
		if err != nil {
			return "", fmt.Errorf("that is not a valid LXS address (%q): %w", line, err)
		}
		addr := a.Hex()
		if err := saveWallet(walletPath, addr, ""); err != nil {
			return "", err
		}
		fmt.Printf("Mining rewards will go to: %s\n", addr)
		return addr, nil
	}

	// Create a fresh wallet. The private key is the ONLY thing that controls the coins,
	// so it is written to disk and the user is told, loudly, to back it up.
	k, err := crypto.GenerateKey()
	if err != nil {
		return "", err
	}
	addr := k.Address().Hex()
	if err := saveWallet(walletPath, addr, k.Hex()); err != nil {
		return "", err
	}
	fmt.Println("=====================================================")
	fmt.Println(" NEW LXS WALLET CREATED")
	fmt.Printf(" Your address (rewards go here): %s\n", addr)
	fmt.Printf(" Private key saved in: %s\n", walletPath)
	fmt.Println(" >>> BACK UP THAT FILE. Whoever holds the key owns the coins.")
	fmt.Println(" >>> Lose it and your mined coins are gone forever.")
	fmt.Println("=====================================================")
	return addr, nil
}

// readSavedAddress returns the address recorded in a wallet file, if the file exists and
// holds a valid one.
func readSavedAddress(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, ln := range strings.Split(string(data), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && f[0] == "address" {
			if a, err := common.AddressFromHex(f[1]); err == nil {
				return a.Hex(), true
			}
		}
	}
	return "", false
}

// saveWallet writes the miner's address (and its private key, when we generated it) to a
// 0600 file. keyHex is "" when the user supplied only an address they hold elsewhere.
func saveWallet(path, addrHex, keyHex string) error {
	var b strings.Builder
	b.WriteString("LXS WALLET — KEEP THIS FILE SAFE. BACK IT UP.\n")
	b.WriteString("Whoever has the private key below controls these coins.\n\n")
	b.WriteString("address      " + addrHex + "\n")
	if keyHex != "" {
		b.WriteString("private key  " + keyHex + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
