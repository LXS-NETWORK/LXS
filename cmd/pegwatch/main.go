// pegwatch is the off-chain operator watcher for the LXS<->Base custodial peg.
//
// It is NOT part of the immutable chain — it is a separate daemon the operator runs,
// changeable anytime. It mirrors the two peg contracts across the two chains:
//
//	LXS PegVault  Locked(nonce, from, amount)  ->  Base WrappedLXS  mint(nonce, from, amount)
//	Base WrappedLXS  Redeem(nonce, from, amount) ->  LXS PegVault   release(nonce, from, amount)
//
// Design follows ChainSafe ChainBridge's proven relayer shape: a listener polls the
// source chain for events, a writer submits the matching call on the destination, a
// blockstore persists how far each side is scanned, and a confirmation depth guards
// against reorgs. Idempotency is enforced ON-CHAIN: each transfer carries a unique
// nonce the destination contract records and refuses to replay, so a restart, a
// re-scan, or a double-submit can NEVER double-mint or double-release. That on-chain
// guarantee is what makes it safe to run an imperfect relayer over real money.
//
// SAFETY: this holds hot keys and moves value. Validate it end-to-end on a testnet
// (two chains, fake funds) before pointing it at real money. The contracts still bound
// a compromised operator (operator-only mint/release, release<=reserve, nonce-once),
// but the operator key is a live secret — protect it.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	"lxs/common"
	"lxs/contracts"
	"lxs/crypto"
	"lxs/rpc"
	"lxs/types"
)

// chain bundles one side's connection, identity, and the peg contract on it.
type chain struct {
	name     string
	chainID  uint64
	contract common.Address
	key      *crypto.PrivateKey
	client   *rpc.Client
}

func (c *chain) head() (uint64, error) {
	var s string
	if err := c.client.Call("eth_blockNumber", &s); err != nil {
		return 0, err
	}
	return hexU64(s), nil
}

func (c *chain) gasPrice() *big.Int {
	var s string
	if err := c.client.Call("eth_gasPrice", &s); err != nil {
		return big.NewInt(1) // fall back to the LXS min; on Base this should not fail
	}
	return hexBig(s)
}

// ethLog is the subset of an eth_getLogs entry we need.
type ethLog struct {
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	TxHash      string   `json:"transactionHash"`
}

func (c *chain) getLogs(topic0 common.Hash, from, to uint64) ([]ethLog, error) {
	filter := map[string]interface{}{
		"address":   c.contract.Hex(),
		"topics":    []string{topic0.Hex()},
		"fromBlock": hexOf(from),
		"toBlock":   hexOf(to),
	}
	var logs []ethLog
	err := c.client.Call("eth_getLogs", &logs, filter)
	return logs, err
}

// submit builds, signs, and sends an eth-legacy tx calling data on the contract. The
// same code path works on both chains: both accept EIP-155 raw txs, and EncodeEthRaw
// produces exactly that.
func (c *chain) submit(data []byte, gasPrice *big.Int) (string, error) {
	var nonceHex string
	if err := c.client.Call("eth_getTransactionCount", &nonceHex, c.key.Address().Hex(), "pending"); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	tx := &types.Transaction{
		Type: types.TxTypeEthLegacy, ChainID: c.chainID, Nonce: hexU64(nonceHex),
		To: &c.contract, Value: new(big.Int), GasLimit: 200_000, GasPrice: gasPrice, Data: data,
	}
	if err := tx.Sign(c.key); err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	raw := "0x" + hex.EncodeToString(tx.EncodeEthRaw())
	var txHash string
	if err := c.client.Call("eth_sendRawTransaction", &txHash, raw); err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	return txHash, nil
}

// processed reports whether the destination contract has already applied `nonce`
// (mintedNonce/releasedNonce). This is the check in check-then-act: we read on-chain
// state as the source of truth, so we never re-submit a done transfer (no confusing
// reverts) and never get stuck (a transfer that did not land is retried). selector is
// mintedNonce(uint256)=a027a1a7 on Base, releasedNonce(uint256)=d06652df on LXS.
func (c *chain) processed(selector []byte, nonce *big.Int) (bool, error) {
	data := "0x" + hex.EncodeToString(append(selector, word(nonce)...))
	var out string
	if err := c.client.Call("eth_call", &out, map[string]interface{}{"to": c.contract.Hex(), "data": data}, "latest"); err != nil {
		return false, err
	}
	return hexBig(out).Sign() != 0, nil // a bool return is 0 (false) or 1 (true)
}

// buildCalldata turns a parsed (nonce, to, amount) event into the destination call.
type buildCalldata func(nonce *big.Int, to common.Address, amount *big.Int) []byte

// relay scans src for topic0 events past the cursor (up to head-confirmations) and
// submits the matching call on dst. On a submit error it stops and does NOT advance
// past the failed event, so the next tick retries it — re-submitting an
// already-applied event is harmless because the destination contract rejects a used
// nonce. Returns the new cursor.
func relay(src, dst *chain, topic0 common.Hash, build buildCalldata, processedSel []byte, confirmations, cursor uint64) uint64 {
	head, err := src.head()
	if err != nil {
		log.Printf("%s: head: %v", src.name, err)
		return cursor
	}
	if head < confirmations {
		return cursor
	}
	safe := head - confirmations
	if safe <= cursor {
		return cursor
	}
	logs, err := src.getLogs(topic0, cursor+1, safe)
	if err != nil {
		log.Printf("%s: getLogs: %v", src.name, err)
		return cursor
	}
	sort.Slice(logs, func(i, j int) bool { return hexU64(logs[i].BlockNumber) < hexU64(logs[j].BlockNumber) })

	gp := dst.gasPrice()
	for _, l := range logs {
		if len(l.Topics) < 3 {
			continue // not the indexed(nonce)+indexed(addr) shape we emit
		}
		nonce := hexBig(l.Topics[1])
		to := topicToAddr(l.Topics[2])
		amount := hexBig(l.Data)

		// Check-then-act: if the destination already applied this nonce, skip it. This
		// makes a re-scan a no-op (no confusing reverts) while on-chain nonce-once is
		// still the hard safety net.
		done, err := dst.processed(processedSel, nonce)
		if err != nil {
			log.Printf("%s->%s nonce %s: processed check failed: %v (retrying)", src.name, dst.name, nonce, err)
			return holdCursor(l, cursor)
		}
		if done {
			continue
		}
		txh, err := dst.submit(build(nonce, to, amount), gp)
		if err != nil {
			log.Printf("relay %s->%s nonce %s FAILED: %v (retrying next tick)", src.name, dst.name, nonce, err)
			return holdCursor(l, cursor)
		}
		log.Printf("relayed %s nonce %s amount %s -> %s tx %s", src.name, nonce, amount, dst.name, txh)
	}
	return safe
}

// holdCursor rewinds to just before the given log's block so it is retried next tick.
func holdCursor(l ethLog, cursor uint64) uint64 {
	if b := hexU64(l.BlockNumber); b > 0 {
		return b - 1
	}
	return cursor
}

// blockStore persists the two cursors so a restart resumes where it left off.
type blockStore struct {
	path string
	LXS  uint64 `json:"lxs"`
	Base uint64 `json:"base"`
}

func loadStore(path string) *blockStore {
	s := &blockStore{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s)
	}
	return s
}

func (s *blockStore) save() {
	data, _ := json.Marshal(s)
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		log.Printf("blockstore save: %v", err)
	}
}

func main() {
	var (
		operatorKeyHex = flag.String("operator-key", "", "operator private key (hot; signs mint on Base and release on LXS)")
		lxsRPC         = flag.String("lxs-rpc", "http://127.0.0.1:8545", "LXS node RPC URL")
		lxsChainID     = flag.Uint64("lxs-chainid", 1337, "LXS chain id")
		vaultHex       = flag.String("vault", "", "PegVault address on LXS")
		baseRPC        = flag.String("base-rpc", "", "Base (or any EVM) RPC URL for the wrapped token")
		baseChainID    = flag.Uint64("base-chainid", 8453, "Base chain id")
		wlxsHex        = flag.String("wlxs", "", "WrappedLXS address on Base")
		confirmations  = flag.Uint64("confirmations", 12, "blocks to wait before acting on an event (reorg safety)")
		storePath      = flag.String("store", "pegwatch-cursors.json", "blockstore path")
		interval       = flag.Duration("interval", 15*time.Second, "poll interval")
	)
	flag.Parse()

	if *operatorKeyHex == "" || *vaultHex == "" || *baseRPC == "" || *wlxsHex == "" {
		fatal("need -operator-key, -vault, -base-rpc and -wlxs")
	}
	key, err := crypto.PrivateKeyFromHex(*operatorKeyHex)
	if err != nil {
		fatal("bad operator key: %v", err)
	}
	vault, err := common.AddressFromHex(*vaultHex)
	if err != nil {
		fatal("bad -vault: %v", err)
	}
	wlxs, err := common.AddressFromHex(*wlxsHex)
	if err != nil {
		fatal("bad -wlxs: %v", err)
	}

	lxs := &chain{name: "LXS", chainID: *lxsChainID, contract: vault, key: key, client: rpc.NewClient(*lxsRPC)}
	base := &chain{name: "Base", chainID: *baseChainID, contract: wlxs, key: key, client: rpc.NewClient(*baseRPC)}
	store := loadStore(*storePath)

	log.Printf("pegwatch: operator %s", key.Address().Hex())
	log.Printf("  LXS  %s  vault %s  (chain %d)", *lxsRPC, vault.Hex(), *lxsChainID)
	log.Printf("  Base %s  wLXS  %s  (chain %d)", *baseRPC, wlxs.Hex(), *baseChainID)
	log.Printf("  confirmations %d, poll %s, cursors lxs=%d base=%d", *confirmations, *interval, store.LXS, store.Base)

	mintOnBase := func(nonce *big.Int, to common.Address, amount *big.Int) []byte {
		return contracts.WlxsMintCalldata(nonce, to, amount)
	}
	releaseOnLXS := func(nonce *big.Int, to common.Address, amount *big.Int) []byte {
		return contracts.PegReleaseCalldata(nonce, to, amount)
	}

	mintedNonceSel := []byte{0xa0, 0x27, 0xa1, 0xa7}   // WrappedLXS.mintedNonce(uint256)
	releasedNonceSel := []byte{0xd0, 0x66, 0x52, 0xdf} // PegVault.releasedNonce(uint256)

	for {
		// LXS Locked -> Base mint (skip if Base already minted this nonce)
		store.LXS = relay(lxs, base, contracts.PegLockedTopic(), mintOnBase, mintedNonceSel, *confirmations, store.LXS)
		// Base Redeem -> LXS release (skip if LXS already released this nonce)
		store.Base = relay(base, lxs, contracts.WlxsRedeemTopic(), releaseOnLXS, releasedNonceSel, *confirmations, store.Base)
		store.save()
		time.Sleep(*interval)
	}
}

// word left-pads a big.Int to a 32-byte EVM word.
func word(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "pegwatch: "+format+"\n", a...)
	os.Exit(1)
}

// --- hex helpers (Ethereum quantities) ---

func hexU64(s string) uint64 { return hexBig(s).Uint64() }

func hexBig(s string) *big.Int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if s == "" {
		return new(big.Int)
	}
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return new(big.Int)
	}
	return n
}

func hexOf(v uint64) string { return "0x" + new(big.Int).SetUint64(v).Text(16) }

func topicToAddr(topic string) common.Address {
	b := hexBig(topic).Bytes() // right-aligned 20 bytes within the 32-byte topic
	var a common.Address
	if len(b) >= 20 {
		copy(a[:], b[len(b)-20:])
	} else {
		copy(a[20-len(b):], b)
	}
	return a
}
