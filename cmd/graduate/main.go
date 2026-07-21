// graduate is the off-chain operator daemon that takes a launchpad coin to a Uniswap
// pool on Base, so it can reach external DEXs / Coinbase. It is NOT part of the immutable
// chain — a separate daemon the operator runs, changeable anytime.
//
// It watches the LXS GraduationVault for Graduated(nonce, coin, from, lxsAmount,
// tokenAmount) and, for each one, builds the Base-side pool:
//
//  1. deploy a WrappedToken (wMeme) for the coin (once per coin; name/symbol read off LXS)
//  2. mint wMeme against the coin locked in the vault           (nonce = gradNonce)
//  3. mint wLXS against the LXS locked in the vault             (nonce = GradWlxsMintNonce)
//  4. approve the Uniswap router for wMeme and wLXS
//  5. addLiquidity(wMeme, wLXS, ...) — creates the pair and seeds it in one call
//
// Design is a RECONCILIATION LOOP, not a fire-and-forget relay: each tick it does at most
// one on-chain action and only after checking on-chain state (is wMeme deployed? has this
// nonce been minted? is the allowance set? is the pool already seeded?). That makes it
// restart-safe and idempotent for the idempotent steps (mints are nonce-guarded on-chain,
// approves are level-set), while the ONE non-idempotent step — addLiquidity — is gated by a
// persisted seeded-set marked only after the tx confirms. Serializing to one action per
// tick keeps the operator's single hot key free of nonce races.
//
// SAFETY: hot key, moves value, and deploys contracts + calls a real DEX. Validate on a
// testnet (Base Sepolia + a local LXS) before pointing it at real money. The GraduationVault
// bounds a compromised operator (locked backing, operator-only release, nonce-once).
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

// chain is one side's connection + identity. Unlike the peg's watcher, graduation touches
// several contracts per side, so the target address is passed per call, not bound here.
type chain struct {
	name    string
	chainID uint64
	key     *crypto.PrivateKey
	client  *rpc.Client
	gasMult uint64 // multiply eth_gasPrice by this for headroom on an L2 base-fee market (Base)
}

func (c *chain) head() (uint64, error) {
	var s string
	if err := c.client.Call("eth_blockNumber", &s); err != nil {
		return 0, err
	}
	return hexU64(s), nil
}

// gasPrice reads the node's suggested price and scales it by gasMult for headroom. On an
// L2 like Base the base fee can rise between reading and mining; a legacy tx priced at the
// bare suggestion can then stall and block the operator's later txs. A small multiple keeps
// it mineable. Overpayment is refunded by the fee market — only the base fee is burned.
func (c *chain) gasPrice() *big.Int {
	var s string
	if err := c.client.Call("eth_gasPrice", &s); err != nil {
		return big.NewInt(1)
	}
	p := hexBig(s)
	if c.gasMult > 1 {
		p.Mul(p, new(big.Int).SetUint64(c.gasMult))
	}
	return p
}

type ethLog struct {
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
}

func (c *chain) getLogs(addr common.Address, topic0 common.Hash, from, to uint64) ([]ethLog, error) {
	filter := map[string]interface{}{
		"address": addr.Hex(), "topics": []string{topic0.Hex()},
		"fromBlock": hexOf(from), "toBlock": hexOf(to),
	}
	var logs []ethLog
	err := c.client.Call("eth_getLogs", &logs, filter)
	return logs, err
}

// callData runs an eth_call against `to` and returns the raw hex return.
func (c *chain) callData(to common.Address, data []byte) (string, error) {
	var out string
	err := c.client.Call("eth_call", &out, map[string]interface{}{"to": to.Hex(), "data": "0x" + hex.EncodeToString(data)}, "latest")
	return out, err
}

// code reports whether a contract is deployed at addr (getCode != "0x").
func (c *chain) code(addr common.Address) bool {
	var out string
	if err := c.client.Call("eth_getCode", &out, addr.Hex(), "latest"); err != nil {
		return false
	}
	return len(strings.TrimPrefix(out, "0x")) > 0
}

// submit builds, signs, and sends an eth-legacy tx. to == nil deploys a contract. Both
// chains accept EIP-155 raw txs, which EncodeEthRaw produces.
func (c *chain) submit(to *common.Address, data []byte, gasLimit uint64, gasPrice *big.Int) (string, error) {
	var nonceHex string
	if err := c.client.Call("eth_getTransactionCount", &nonceHex, c.key.Address().Hex(), "pending"); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	tx := &types.Transaction{
		Type: types.TxTypeEthLegacy, ChainID: c.chainID, Nonce: hexU64(nonceHex),
		To: to, Value: new(big.Int), GasLimit: gasLimit, GasPrice: gasPrice, Data: data,
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

type ethReceipt struct {
	ContractAddress string `json:"contractAddress"`
	Status          string `json:"status"`
	BlockNumber     string `json:"blockNumber"`
}

// receipt returns the mined receipt, or ok=false if the tx is not mined yet.
func (c *chain) receipt(txHash string) (*ethReceipt, bool) {
	var r *ethReceipt
	if err := c.client.Call("eth_getTransactionReceipt", &r, txHash); err != nil || r == nil || r.BlockNumber == "" {
		return nil, false
	}
	return r, true
}

// nonceUsed reads a WrappedToken/WrappedLXS mintedNonce(nonce) — true once that mint applied.
func (c *chain) nonceUsed(token common.Address, nonce *big.Int) (bool, error) {
	out, err := c.callData(token, append([]byte{0xa0, 0x27, 0xa1, 0xa7}, word(nonce)...)) // mintedNonce(uint256)
	if err != nil {
		return false, err
	}
	return hexBig(out).Sign() != 0, nil
}

// allowance reads ERC-20 allowance(owner, spender).
func (c *chain) allowance(token, owner, spender common.Address) (*big.Int, error) {
	data := append([]byte{0xdd, 0x62, 0xed, 0x3e}, addr32(owner)...) // allowance(address,address)
	data = append(data, addr32(spender)...)
	out, err := c.callData(token, data)
	if err != nil {
		return nil, err
	}
	return hexBig(out), nil
}

// readString reads a string getter (name()/symbol()) and decodes the ABI return.
func (c *chain) readString(to common.Address, selector []byte) string {
	out, err := c.callData(to, selector)
	if err != nil {
		return ""
	}
	b := hexBytes(out)
	if len(b) < 64 {
		return ""
	}
	n := new(big.Int).SetBytes(b[32:64]).Int64()
	if n < 0 || int64(len(b)) < 64+n {
		return ""
	}
	return string(b[64 : 64+n])
}

// grad is a parsed Graduated event.
type grad struct {
	nonce       *big.Int
	coin        common.Address
	lxsAmount   *big.Int
	tokenAmount *big.Int
}

func parseGraduated(l ethLog) (grad, bool) {
	if len(l.Topics) < 4 {
		return grad{}, false // sig + indexed nonce, coin, from
	}
	data := hexBytes(l.Data)
	if len(data) < 64 {
		return grad{}, false
	}
	return grad{
		nonce:       hexBig(l.Topics[1]),
		coin:        topicToAddr(l.Topics[2]),
		lxsAmount:   new(big.Int).SetBytes(data[0:32]),
		tokenAmount: new(big.Int).SetBytes(data[32:64]),
	}, true
}

// store persists everything needed to resume mid-graduation across restarts.
type store struct {
	path     string
	Cursor   uint64            `json:"cursor"`   // LXS scan position
	Wrapped  map[string]string `json:"wrapped"`  // coin -> deployed wMeme address
	DeployTx map[string]string `json:"deployTx"` // coin -> in-flight wMeme deploy tx
	Seed     map[string]string `json:"seed"`     // gradNonce -> in-flight addLiquidity tx
	Seeded   map[string]bool   `json:"seeded"`   // gradNonce -> pool confirmed seeded
}

func loadStore(path string) *store {
	s := &store{path: path, Wrapped: map[string]string{}, DeployTx: map[string]string{}, Seed: map[string]string{}, Seeded: map[string]bool{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s)
		for _, m := range []*map[string]string{&s.Wrapped, &s.DeployTx, &s.Seed} {
			if *m == nil {
				*m = map[string]string{}
			}
		}
		if s.Seeded == nil {
			s.Seeded = map[string]bool{}
		}
	}
	return s
}

func (s *store) save() {
	data, _ := json.Marshal(s)
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		log.Printf("store save: %v", err)
	}
}

// advance does AT MOST ONE on-chain action for graduation g, returning true if it acted.
// Every step first checks on-chain state, so a restart or a re-run resumes exactly where
// it left off and never repeats a completed step.
func advance(lxs, base *chain, router, wlxs common.Address, gp *big.Int, s *store, g grad) bool {
	op := base.key.Address()
	nkey := g.nonce.String()
	ckey := g.coin.Hex()
	if s.Seeded[nkey] {
		return false // fully done
	}

	// If a pool-seed tx is already in flight, resolve it before anything else — otherwise
	// the earlier steps re-run (addLiquidity consumes the approvals) and waste txs while it
	// confirms. This is the ONE non-idempotent step, so it is gated on a confirmed receipt.
	if txh := s.Seed[nkey]; txh != "" {
		r, mined := base.receipt(txh)
		if !mined {
			return false
		}
		delete(s.Seed, nkey)
		if r.Status == "0x0" {
			log.Printf("grad %s: addLiquidity reverted, retrying", nkey) // approvals survive a revert; safe to retry
			return true
		}
		s.Seeded[nkey] = true
		log.Printf("grad %s: POOL SEEDED (wMeme/wLXS) — coin %s is now on a Base DEX", nkey, ckey)
		return true
	}

	// Step 1: ensure the wMeme is deployed (once per coin).
	wmemeHex := s.Wrapped[ckey]
	if wmemeHex == "" {
		if txh := s.DeployTx[ckey]; txh != "" {
			r, mined := base.receipt(txh)
			if !mined {
				return false // waiting for the deploy to mine
			}
			if r.Status == "0x0" || r.ContractAddress == "" {
				delete(s.DeployTx, ckey) // deploy reverted — retry with a fresh tx next tick
				log.Printf("grad %s: wMeme deploy reverted, retrying", nkey)
				return true
			}
			s.Wrapped[ckey] = r.ContractAddress
			delete(s.DeployTx, ckey)
			log.Printf("grad %s: wMeme for %s deployed at %s", nkey, ckey, r.ContractAddress)
			return true
		}
		name := lxs.readString(g.coin, []byte{0x06, 0xfd, 0xde, 0x03})   // name()
		symbol := lxs.readString(g.coin, []byte{0x95, 0xd8, 0x9b, 0x41}) // symbol()
		if name == "" {
			name, symbol = "LXS Coin", "LXSC" // coin metadata unreadable; still deployable
		}
		txh, err := base.submit(nil, contracts.WrappedTokenInit(op, name, symbol), 2_000_000, gp)
		if err != nil {
			log.Printf("grad %s: wMeme deploy submit failed: %v", nkey, err)
			return false
		}
		s.DeployTx[ckey] = txh
		log.Printf("grad %s: deploying wMeme %q/%q tx %s", nkey, name, symbol, txh)
		return true
	}
	wmeme, err := common.AddressFromHex(wmemeHex)
	if err != nil {
		log.Printf("grad %s: bad stored wMeme %s: %v", nkey, wmemeHex, err)
		return false
	}
	if !base.code(wmeme) {
		return false // deploy tx mined but code not visible yet; wait a tick
	}

	// Step 2: mint wMeme against the locked coin (nonce-guarded on-chain).
	if used, err := base.nonceUsed(wmeme, g.nonce); err != nil {
		log.Printf("grad %s: wMeme mintedNonce read: %v", nkey, err)
		return false
	} else if !used {
		if txh, err := base.submit(&wmeme, contracts.WrappedTokenMintCalldata(g.nonce, op, g.tokenAmount), 200_000, gp); err != nil {
			log.Printf("grad %s: mint wMeme failed: %v", nkey, err)
		} else {
			log.Printf("grad %s: minted wMeme %s tx %s", nkey, g.tokenAmount, txh)
		}
		return true
	}

	// Step 3: mint wLXS against the locked LXS (disjoint nonce range from the peg).
	wlxsNonce := contracts.GradWlxsMintNonce(g.nonce)
	if used, err := base.nonceUsed(wlxs, wlxsNonce); err != nil {
		log.Printf("grad %s: wLXS mintedNonce read: %v", nkey, err)
		return false
	} else if !used {
		if txh, err := base.submit(&wlxs, contracts.WlxsMintCalldata(wlxsNonce, op, g.lxsAmount), 200_000, gp); err != nil {
			log.Printf("grad %s: mint wLXS failed: %v", nkey, err)
		} else {
			log.Printf("grad %s: minted wLXS %s tx %s", nkey, g.lxsAmount, txh)
		}
		return true
	}

	// Step 4: approve the router for wMeme.
	if a, err := base.allowance(wmeme, op, router); err != nil {
		log.Printf("grad %s: wMeme allowance read: %v", nkey, err)
		return false
	} else if a.Cmp(g.tokenAmount) < 0 {
		if txh, err := base.submit(&wmeme, contracts.ApproveCalldata(router, g.tokenAmount), 100_000, gp); err != nil {
			log.Printf("grad %s: approve wMeme failed: %v", nkey, err)
		} else {
			log.Printf("grad %s: approved wMeme->router tx %s", nkey, txh)
		}
		return true
	}

	// Step 5: approve the router for wLXS.
	if a, err := base.allowance(wlxs, op, router); err != nil {
		log.Printf("grad %s: wLXS allowance read: %v", nkey, err)
		return false
	} else if a.Cmp(g.lxsAmount) < 0 {
		if txh, err := base.submit(&wlxs, contracts.ApproveCalldata(router, g.lxsAmount), 100_000, gp); err != nil {
			log.Printf("grad %s: approve wLXS failed: %v", nkey, err)
		} else {
			log.Printf("grad %s: approved wLXS->router tx %s", nkey, txh)
		}
		return true
	}

	// Step 6: submit the pool seed (its confirmation is handled at the top of advance).
	deadline := new(big.Int).Lsh(big.NewInt(1), 62) // far-future; refine to a real timestamp in prod
	// mins at 0: on a fresh pair the ratio we set IS the opening price, so there is nothing to slip against.
	data := contracts.UniV2AddLiquidityCalldata(wmeme, wlxs, g.tokenAmount, g.lxsAmount, big.NewInt(0), big.NewInt(0), op, deadline)
	txh, err := base.submit(&router, data, 4_000_000, gp) // first-time createPair is gas-heavy
	if err != nil {
		log.Printf("grad %s: addLiquidity submit failed: %v", nkey, err)
		return false
	}
	s.Seed[nkey] = txh
	log.Printf("grad %s: seeding pool tx %s", nkey, txh)
	return true
}

func main() {
	var (
		operatorKeyHex = flag.String("operator-key", "", "operator private key (hot; signs on Base and reads LXS)")
		lxsRPC         = flag.String("lxs-rpc", "http://127.0.0.1:8545", "LXS node RPC URL")
		lxsChainID     = flag.Uint64("lxs-chainid", 1337, "LXS chain id")
		vaultHex       = flag.String("vault", "", "GraduationVault address on LXS")
		baseRPC        = flag.String("base-rpc", "", "Base (or any EVM) RPC URL")
		baseChainID    = flag.Uint64("base-chainid", 8453, "Base chain id")
		wlxsHex        = flag.String("wlxs", "", "WrappedLXS address on Base (the shared wLXS)")
		routerHex      = flag.String("router", contracts.UniV2Router02Base, "UniswapV2Router02 address on Base")
		confirmations  = flag.Uint64("confirmations", 12, "blocks to wait before acting on a Graduated event (use 0 on a local node that skips empty blocks)")
		gasMult        = flag.Uint64("base-gas-mult", 2, "multiply the suggested Base gas price by this for headroom on the L2 base-fee market")
		storePath      = flag.String("store", "graduate-state.json", "state file path")
		interval       = flag.Duration("interval", 15*time.Second, "poll/step interval")
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
	router, err := common.AddressFromHex(*routerHex)
	if err != nil {
		fatal("bad -router: %v", err)
	}

	lxs := &chain{name: "LXS", chainID: *lxsChainID, key: key, client: rpc.NewClient(*lxsRPC)}
	base := &chain{name: "Base", chainID: *baseChainID, key: key, client: rpc.NewClient(*baseRPC), gasMult: *gasMult}
	s := loadStore(*storePath)

	log.Printf("graduate: operator %s", key.Address().Hex())
	log.Printf("  LXS  %s  vault %s (chain %d)", *lxsRPC, vault.Hex(), *lxsChainID)
	log.Printf("  Base %s  wLXS %s  router %s (chain %d)", *baseRPC, wlxs.Hex(), router.Hex(), *baseChainID)
	log.Printf("  confirmations %d, interval %s, cursor %d", *confirmations, *interval, s.Cursor)

	topic := contracts.GraduatedTopic()
	for {
		step(lxs, base, vault, router, wlxs, topic, *confirmations, s)
		s.save()
		time.Sleep(*interval)
	}
}

// step scans for Graduated events and advances the oldest not-yet-seeded graduation by one
// on-chain action, so the operator's single key never races itself. The scan cursor only
// advances past events that are fully seeded, so nothing is skipped before its pool exists.
func step(lxs, base *chain, vault, router, wlxs common.Address, topic common.Hash, confirmations uint64, s *store) {
	head, err := lxs.head()
	if err != nil {
		log.Printf("LXS head: %v", err)
		return
	}
	if head < confirmations {
		return
	}
	safe := head - confirmations
	from := s.Cursor + 1
	if s.Cursor == 0 {
		from = 0
	}
	if safe < from {
		return
	}
	logs, err := lxs.getLogs(vault, topic, from, safe)
	if err != nil {
		log.Printf("LXS getLogs: %v", err)
		return
	}
	sort.Slice(logs, func(i, j int) bool { return hexU64(logs[i].BlockNumber) < hexU64(logs[j].BlockNumber) })

	grads := make([]grad, 0, len(logs))
	for _, l := range logs {
		if g, ok := parseGraduated(l); ok {
			grads = append(grads, g)
		}
	}
	gp := base.gasPrice()
	// advance the first graduation still needing work; one action per tick.
	for _, g := range grads {
		if s.Seeded[g.nonce.String()] {
			continue
		}
		if advance(lxs, base, router, wlxs, gp, s, g) {
			return // acted; persist and come back next tick
		}
	}
	// every graduation up to `safe` is seeded — safe to advance the cursor.
	allSeeded := true
	for _, g := range grads {
		if !s.Seeded[g.nonce.String()] {
			allSeeded = false
			break
		}
	}
	if allSeeded {
		s.Cursor = safe
	}
}

// --- helpers ---

func word(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func addr32(a common.Address) []byte {
	out := make([]byte, 32)
	copy(out[32-len(a):], a[:])
	return out
}

func hexBytes(s string) []byte {
	b, _ := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	return b
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "graduate: "+format+"\n", a...)
	os.Exit(1)
}

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
	b := hexBig(topic).Bytes()
	var a common.Address
	if len(b) >= 20 {
		copy(a[:], b[len(b)-20:])
	} else {
		copy(a[20-len(b):], b)
	}
	return a
}
