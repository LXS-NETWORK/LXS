package main

import (
	"lxs/store"

	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"lxs/common"
	"lxs/contracts"
	"lxs/core"
	"lxs/crypto"
	"lxs/health"
	"lxs/mempool"
	"lxs/node"
	"lxs/rpc"
	"lxs/state"
	"lxs/types"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "init":
		err = initGenesis(os.Args[2:])
	case "node":
		err = runNode(os.Args[2:])
	case "mine":
		err = mineCmd(os.Args[2:])
	case "p2p-id":
		err = p2pID(os.Args[2:])
	case "balance":
		err = balance(os.Args[2:])
	case "send":
		err = send(os.Args[2:])
	case "call":
		err = callContract(os.Args[2:])
	case "create-token":
		err = createToken(os.Args[2:])
	case "launch-coin":
		err = launchCoin(os.Args[2:])
	case "block":
		err = getBlock(os.Args[2:])
	case "receipt":
		err = getReceipt(os.Args[2:])
	case "demo":
		err = demo()
	case "reorg-demo":
		err = reorgDemo()
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Printf("%s — an L1 from scratch. phases 1-3.5: node, JSON-RPC, reorg, persistence\n", common.NetworkName)
	fmt.Println(`

node commands
  init      write lxs-genesis.json funding a fresh account
  node      run a node with JSON-RPC
  keygen    generate a keypair

wallet commands (talk to a running node)
  balance   -addr ADDRESS
  send      -key HEX -to ADDRESS -value N [-gas-price N]
            -key HEX -data HEX -gas N [-to ADDR]   (deploy/call a contract)
  call      -to ADDRESS -data HEX [-from ADDR]     (read-only, no tx)
  create-token -key HEX -name "My Coin" -symbol MYC -supply N
  block     -n HEIGHT | -hash HASH  [-full]
  receipt   -tx HASH

  demo        run an in-memory chain end to end, no server
  reorg-demo  build two competing branches and watch a reorg`)
}

const defaultRPC = "http://127.0.0.1:8545"

func keygen() error {
	k, err := crypto.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Printf("private key  %s\n", k.Hex())
	fmt.Printf("address      %s\n", k.Address().Hex())
	return nil
}

func initGenesis(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("out", "lxs-genesis.json", "output path")
	supply := fs.Int64("supply", 20_000_000, "whole LXS pre-mined at genesis (Bitcoin-model: only the founder's stake; the ~80M rest is EARNED via mining)")
	founderPct := fs.Int64("founder-pct", 100, "percent of the genesis pre-mine to the founder wallet (100 = no treasury alloc; the rest of supply comes from mining)")
	founderAddrHex := fs.String("founder-addr", "", "founder address that receives the genesis pre-mine (pass YOUR MetaMask address — PUBLIC only, the key stays in MetaMask). Empty = generate a throwaway key (devnet)")
	chainID := fs.Uint64("chain-id", common.DevnetChainID, "chain id")
	// Starting PoW difficulty. ~2M is roughly one block per couple of seconds on
	// a laptop; per-block adjustment tracks the target from there. Difficulty 1
	// makes mining instant — fine for a throwaway node, floods a devnet.
	difficulty := fs.Uint64("difficulty", 2_000_000, "initial mining difficulty")
	treasuryPct := fs.Int64("treasury-pct", 0, "percent of EVERY block reward sent to the treasury (0 = off; the miner keeps the rest + fees)")
	treasuryAddr := fs.String("treasury-addr", "", `treasury destination: an address, or "burn" to DESTROY the cut (deflation, no key needed)`)
	fs.Parse(args)

	if *founderPct < 0 || *founderPct > 100 {
		return fmt.Errorf("founder-pct must be between 0 and 100, got %d", *founderPct)
	}

	// Block-reward split: a consensus parameter baked into genesis, so it must be
	// set now — it cannot be added post-launch without a hard fork. "burn" routes
	// the cut to destruction (deflation); an address routes it to a treasury.
	var treasuryReward common.Address
	var treasuryBps uint64
	if *treasuryPct > 0 {
		if *treasuryPct > 100 {
			return fmt.Errorf("treasury-pct must be between 0 and 100, got %d", *treasuryPct)
		}
		treasuryBps = uint64(*treasuryPct) * 100
		switch *treasuryAddr {
		case "":
			return errors.New(`treasury-pct is set but treasury-addr is empty — pass -treasury-addr burn (to auto-burn) or an address`)
		case "burn":
			treasuryReward = common.BurnAddress
		default:
			a, err := common.AddressFromHex(*treasuryAddr)
			if err != nil {
				return fmt.Errorf("bad treasury-addr: %w", err)
			}
			treasuryReward = a
		}
	}

	// The founder address receives the pre-mine. With -founder-addr no key is
	// generated or stored here (an external wallet holds it; only the public
	// address is passed). Empty falls back to a throwaway generated key (devnet).
	var founderAddr common.Address
	var founderKey *crypto.PrivateKey
	if *founderAddrHex != "" {
		a, err := common.AddressFromHex(*founderAddrHex)
		if err != nil {
			return fmt.Errorf("bad -founder-addr: %w", err)
		}
		founderAddr = a
	} else {
		k, err := crypto.GenerateKey()
		if err != nil {
			return err
		}
		founderKey = k
		founderAddr = k.Address()
	}
	rest, err := crypto.GenerateKey()
	if err != nil {
		return err
	}

	total := common.LXS(*supply)
	// Integer arithmetic. The remainder is derived (total - founderShare), not
	// computed independently: two separate divisions would round apart and the sum
	// would miss the total by a few lux, which Validate rejects. Deriving it makes
	// the sum exact.
	founderShare := new(big.Int).Div(new(big.Int).Mul(total, big.NewInt(*founderPct)), big.NewInt(100))
	remainder := new(big.Int).Sub(total, founderShare)

	alloc := map[common.Address]*core.BigStr{}
	if founderShare.Sign() > 0 {
		alloc[founderAddr] = &core.BigStr{Int: founderShare}
	}
	if remainder.Sign() > 0 {
		alloc[rest.Address()] = &core.BigStr{Int: remainder}
	}

	g := &core.Genesis{
		Name:              common.NetworkName,
		ChainID:           *chainID,
		Timestamp:         1700000000000,
		GasLimit:          30_000_000,
		Alloc:             alloc,
		TotalSupply:       &core.BigStr{Int: total},
		Difficulty:        *difficulty,
		TreasuryReward:    treasuryReward,
		TreasuryRewardBps: treasuryBps,
	}
	if err := g.Validate(); err != nil {
		return err
	}
	if err := g.Save(*path); err != nil {
		return err
	}
	_, block := g.Build()

	fmt.Printf("wrote %s\n\n", *path)
	fmt.Printf("network      %s ($%s)\n", common.NetworkName, common.Ticker)
	fmt.Printf("chain id     %d\n", g.ChainID)
	fmt.Printf("genesis hash %s\n", block.Hash().Hex())
	fmt.Printf("state root   %s\n", block.Header.StateRoot.Hex())
	rewardWhole := new(big.Int).Div(state.BaseBlockReward, common.OneLXS)
	minedTotal := new(big.Int).Mul(rewardWhole, new(big.Int).SetUint64(2*state.HalvingInterval)) // geometric sum ≈ 2*base*interval
	fmt.Printf("supply       %d %s at genesis; mining adds %s %s/block, halving every %d blocks (≈%s %s mined total)\n",
		*supply, common.Ticker, rewardWhole, common.Ticker,
		state.HalvingInterval, minedTotal, common.Ticker)
	fmt.Printf("difficulty   %d (adjusts toward one block / %v)\n", *difficulty, time.Duration(core.TargetBlockTime)*time.Millisecond)
	switch {
	case treasuryBps == 0:
		fmt.Printf("reward split 100%% to the miner (no treasury cut)\n\n")
	case treasuryReward == common.BurnAddress:
		fmt.Printf("reward split %d%% of every block reward is BURNED (deflation); miner keeps %d%% + fees\n\n", treasuryBps/100, 100-treasuryBps/100)
	default:
		fmt.Printf("reward split %d%% of every block reward -> treasury %s; miner keeps %d%% + fees\n\n", treasuryBps/100, treasuryReward.Hex(), 100-treasuryBps/100)
	}

	if founderKey != nil {
		fmt.Printf("founder wallet — %d%% pre-mine (%s %s). SAVE THIS, it is stored nowhere:\n",
			*founderPct, new(big.Int).Div(founderShare, common.OneLXS), common.Ticker)
		fmt.Printf("  address    %s\n", founderAddr.Hex())
		fmt.Printf("  private    %s\n\n", founderKey.Hex())
	} else {
		fmt.Printf("founder wallet — %d%% pre-mine (%s %s) → YOUR address (you hold the key, e.g. in MetaMask):\n",
			*founderPct, new(big.Int).Div(founderShare, common.OneLXS), common.Ticker)
		fmt.Printf("  address    %s\n\n", founderAddr.Hex())
	}

	if remainder.Sign() > 0 {
		fmt.Printf("remaining %s %s — SAVE THIS TOO:\n",
			new(big.Int).Div(remainder, common.OneLXS), common.Ticker)
		fmt.Printf("  address    %s\n", rest.Address().Hex())
		fmt.Printf("  private    %s\n", rest.Hex())
	}
	return nil
}

// mineCmd is the miner-facing entry point (the "click Mine" experience in CLI form). It runs
// a node with the defaults a miner wants baked in — mine + empty-blocks — so a miner earns
// continuously (the producer skips empty blocks otherwise, and a quiet chain would never pay
// out). Progress is the node's text log. The native double-click app just wraps this command,
// so the right flags can never be forgotten.
func mineCmd(args []string) error {
	fs := flag.NewFlagSet("mine", flag.ExitOnError)
	coinbase := fs.String("coinbase", "", "YOUR LXS address — every block reward is paid here (required)")
	bootstrap := fs.String("bootstrap", "", "seed node multiaddr to join the network (needs a libp2p build + -p2p-port)")
	genesis := fs.String("genesis", "lxs-genesis.json", "genesis file (the network's identity)")
	dataDir := fs.String("datadir", "", "database directory (empty = in-memory, lost on exit)")
	rpcAddr := fs.String("rpc", "127.0.0.1:8545", "rpc listen address")
	p2pPort := fs.Int("p2p-port", 0, "libp2p listen port (0 = no networking; needed to join a real network)")
	fs.Parse(args)

	if *coinbase == "" {
		return errors.New("-coinbase <your LXS address> is required — that is where your mining rewards go")
	}

	// Translate to the node command with the miner defaults ON. -empty-blocks is the key one:
	// without it the producer only mines when there are txs, so a miner on a quiet chain earns
	// nothing. A real PoW miner (Bitcoin-style) always mines, txs or not.
	nodeArgs := []string{
		"-mine", "-empty-blocks",
		"-coinbase", *coinbase,
		"-genesis", *genesis,
		"-rpc", *rpcAddr,
	}
	if *dataDir != "" {
		nodeArgs = append(nodeArgs, "-datadir", *dataDir)
	}
	if *bootstrap != "" {
		nodeArgs = append(nodeArgs, "-bootstrap", *bootstrap)
	}
	if *p2pPort != 0 {
		nodeArgs = append(nodeArgs, "-p2p-port", fmt.Sprintf("%d", *p2pPort))
	}

	fmt.Printf("mining to %s\n", *coinbase)
	if *p2pPort == 0 {
		fmt.Printf("note: no -p2p-port set, so this mines a LOCAL chain only. Add -p2p-port and -bootstrap to join the network.\n")
	}
	return runNode(nodeArgs)
}

func runNode(args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	genesisPath := fs.String("genesis", "lxs-genesis.json", "genesis file")
	rpcAddr := fs.String("rpc", "127.0.0.1:8545", "rpc listen address")
	dataDir := fs.String("datadir", "", "database directory (empty = in-memory, chain is lost on exit)")
	retention := fs.Uint64("retention", 0, "reorg depth limit / in-memory state window (0 = default)")
	blockTime := fs.Duration("block-time", 2*time.Second, "block interval")
	mine := fs.Bool("mine", false, "produce (mine) blocks")
	emptyBlocks := fs.Bool("empty-blocks", false, "produce blocks even with no txs")
	p2pPort := fs.Int("p2p-port", 0, "libp2p listen port (0 = disabled)")
	p2pSeed := fs.String("p2p-seed", "", "seed for a deterministic p2p identity (devnet; empty = ephemeral)")
	bootstrapCSV := fs.String("bootstrap", "", "comma-separated peer multiaddrs to dial on startup")
	coinbaseHex := fs.String("coinbase", "", "fee recipient (default: a random address)")
	rpcAPIKeysCSV := fs.String("rpc-api-keys", "", "comma-separated API keys required on the RPC port (empty = open; prefer env LXS_RPC_API_KEYS — argv is visible in `ps`)")
	rpcCORSCSV := fs.String("rpc-cors", "", "comma-separated browser origins allowed to read the RPC (empty = none; \"*\" = any)")
	minGasPrice := fs.Uint64("min-gas-price", 1, "reject txs priced below this at admission (spam floor; policy, NOT consensus — a cheaper tx in a block is still valid). 0 = accept any")
	faucet := fs.Bool("faucet", false, "run a /faucet endpoint that gives a NEW address enough LXS to create its first token. Funded by <datadir>/faucet.key — FUND that address; the tap runs dry at exactly that balance")
	faucetAmountWei := fs.Uint64("faucet-amount", 7_000_000, "wei per faucet claim. Default 7,000,000 wei = the REAL gas for ~5 token creations at gasPrice 1 (measured: a launchpad create burns ~1.02M gas; the create tx is submitted with a ~1.3M limit, unused refunded). Not 1 LXS — just the gas dust needed. Per-IP rate limited + one claim per address")
	fs.Parse(args)

	var bootstrap []string
	for _, a := range strings.Split(*bootstrapCSV, ",") {
		if a = strings.TrimSpace(a); a != "" {
			bootstrap = append(bootstrap, a)
		}
	}

	// A secret on argv is readable by any local user via ps, so the env var is the
	// channel for a real key; the flag is a devnet convenience. Both are honoured.
	var apiKeys []string
	for _, src := range []string{*rpcAPIKeysCSV, os.Getenv("LXS_RPC_API_KEYS")} {
		for _, k := range strings.Split(src, ",") {
			if k = strings.TrimSpace(k); k != "" {
				apiKeys = append(apiKeys, k)
			}
		}
	}

	var corsOrigins []string
	for _, o := range strings.Split(*rpcCORSCSV, ",") {
		if o = strings.TrimSpace(o); o != "" {
			corsOrigins = append(corsOrigins, o)
		}
	}

	g, err := core.LoadGenesis(*genesisPath)
	if err != nil {
		return fmt.Errorf("loading genesis: %w", err)
	}

	var coinbase common.Address
	if *coinbaseHex != "" {
		coinbase, err = common.AddressFromHex(*coinbaseHex)
		if err != nil {
			return err
		}
	} else {
		k, _ := crypto.GenerateKey()
		coinbase = k.Address()
	}

	var db store.KV
	if *dataDir == "" {
		db = store.NewMemory()
		fmt.Printf("datadir      (in-memory — the chain is lost on exit)\n")
	} else {
		db, err = openDB(*dataDir)
		if err != nil {
			return err
		}
		fmt.Printf("datadir      %s\n", *dataDir)
	}

	bc, err := core.NewBlockchain(db, g, core.Options{Retention: *retention})
	if err != nil {
		return err
	}
	defer bc.Close()

	pool := mempool.New(8192)
	if *minGasPrice > 0 {
		pool.SetMinGasPrice(new(big.Int).SetUint64(*minGasPrice)) // admission spam floor
	}
	// Without this a reorg silently destroys every tx in the orphaned blocks:
	// they left the pool when mined and nothing returns them.
	core.BindMempool(bc, pool)
	prod := core.NewProducer(bc, pool, coinbase)

	fmt.Printf("chain id     %d\n", bc.ChainID())
	fmt.Printf("head         %s  height %d\n", short(bc.Head().Hash().Hex()), bc.Head().Height())
	fmt.Printf("coinbase     %s\n", coinbase.Hex())
	fmt.Printf("fork choice  %s\n", bc.ForkChoice().Name())
	fmt.Printf("retention    %d blocks (reorgs deeper than this are refused)\n", bc.Retention())
	if len(apiKeys) > 0 {
		fmt.Printf("rpc auth     ON — %d key(s) required (Authorization: Bearer …)\n", len(apiKeys))
	} else {
		fmt.Printf("rpc auth     OFF — the RPC port is open to anyone who can reach it\n")
	}
	if *mine {
		fmt.Printf("role         PRODUCER (block time %s)\n", *blockTime)
	} else {
		fmt.Printf("role         follower (not producing)\n")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Periodically drop txs that can no longer execute (nonce below the committed head),
	// so the pool does not silently fill over time with un-minable dead weight that
	// Pending skips but that still holds slots.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		checks := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pool.Demote(bc.StateSnapshot())
				// Periodic value-conservation self-check (every ~10 min). A violation
				// is a deterministic supply bug that inflates silently; log it LOUD so
				// the operator and the health monitor see it, rather than let it ride.
				if checks++; checks%20 == 0 {
					if err := bc.CheckConservation(); err != nil {
						log.Printf("CRITICAL: %v", err)
					}
				}
			}
		}
	}()

	// Wire p2p before the producer seals: SetOnBlock must be set so the first
	// block produced is also the first announced.
	var txBroadcast func(*types.Transaction) error
	var peersProbe func() []health.PeerHealth
	var p2pResync, p2pRedial func(context.Context) error
	var p2pDisconnect func(string) error
	if *p2pPort != 0 {
		h, err := startP2P(ctx, bc, pool, prod, *p2pPort, *p2pSeed, bootstrap, *dataDir)
		if err != nil {
			return err
		}
		defer h.Close()
		txBroadcast = h.Broadcast
		peersProbe = h.Peers
		p2pResync, p2pRedial = h.Resync, h.Redial
		p2pDisconnect = h.Disconnect
		fmt.Printf("p2p          libp2p on port %d — chain %d, %d bootstrap peer(s)\n", *p2pPort, bc.ChainID(), len(bootstrap))
	} else {
		fmt.Printf("p2p          disabled — this node is alone\n")
	}
	fmt.Println()

	// Health monitor: an always-on loop tracking the node's own vitals plus what
	// it observes of peers it gossips with. The peers seam is the p2p view (count
	// + each peer's Scorer penalty/ban); nil when p2p is off, so a standalone node
	// is not flagged as critically isolated.
	healthMon := &health.Monitor{
		Height:   func() uint64 { return bc.Head().Height() },
		HeadTime: func() time.Time { return time.UnixMilli(int64(bc.Head().Header.Timestamp)) },
		Mempool:  pool.Len,
		Peers:    peersProbe, // nil-safe: no p2p => no peer observation, no false alarm
		// Self-diagnosis: the node's own resources + data layer.
		Goroutines: runtime.NumGoroutine,
		StoreOK: func() error { // a real read through the store, so a broken DB surfaces
			h := bc.Head()
			if h == nil {
				return errors.New("nil head")
			}
			_, err := bc.BlockByHeight(h.Height())
			return err
		},
		Now:       time.Now,
		StartedAt: time.Now(),
		Producing: *mine,
	}
	if *dataDir != "" { // disk is only meaningful with a persistent datadir
		healthMon.DiskFree = func() (uint64, uint64) { return diskFree(*dataDir) }
	}
	// Healer: reacts to an unhealthy snapshot — resync from peers on a stalled
	// head, redial bootstrap when isolated — throttled by a cooldown, and a no-op
	// when a remedy is not wired (no p2p). Driven by the Reporter's per-tick hook.
	healer := &health.Healer{
		Resync:           p2pResync,
		Redial:           p2pRedial,
		StaleResyncAfter: 90 * time.Second,
		Cooldown:         60 * time.Second,
		// Reclaim resources the node diagnosed in itself. GC + FreeOSMemory is
		// always safe and cannot corrupt state.
		Relieve: func(context.Context) error {
			runtime.GC()
			debug.FreeOSMemory()
			return nil
		},
		Now: time.Now,
		Log: log.Printf,
	}
	// Governor: a rule-based posture engine. Rules map the health snapshot to a
	// posture (normal/guarded/lockdown); the lever is the warden's sweep cadence —
	// tighter under attack, relaxed when calm. Changes are recorded (governor.Audit).
	var wardenIntervalNs int64 = int64(20 * time.Second)
	wardenPoke := make(chan struct{}, 1)
	postureInterval := map[health.Posture]time.Duration{"normal": 20 * time.Second, "guarded": 8 * time.Second, "lockdown": 3 * time.Second}
	governor := &health.Governor{
		Rules: []health.GovRule{
			{Posture: "lockdown", Reason: "coordinated peer attack (many banned)", Match: func(s health.Snapshot) bool {
				// Ratio OR an absolute count: a sybil flood of fresh-identity, not-yet-banned
				// peers inflates PeerCount and dilutes the ratio, so the ratio alone would
				// never fire during the very attack it targets. The absolute threshold fires
				// regardless of how many unbanned connections mask it.
				return (s.PeerCount > 0 && s.BannedPeers*2 >= s.PeerCount) || s.BannedPeers >= 20
			}},
			{Posture: "guarded", Reason: "node not fully healthy", Match: func(s health.Snapshot) bool {
				return s.Status != health.StatusOK
			}},
		},
		Default: "normal",
		Apply: func(p health.Posture) {
			if d, ok := postureInterval[p]; ok {
				atomic.StoreInt64(&wardenIntervalNs, int64(d))
			}
			select { // wake the warden to apply the new cadence (non-blocking)
			case wardenPoke <- struct{}{}:
			default:
			}
		},
		Now: time.Now, Log: log.Printf,
	}

	reporter := &health.Reporter{
		Monitor: healthMon, Every: 30 * time.Second, Log: log.Printf,
		OnTick: func(ctx context.Context, s health.Snapshot) {
			healer.Handle(ctx, s) // react to trouble
			governor.Evaluate(s)  // set the governed posture
		},
	}
	go func() { _ = reporter.Run(ctx) }()
	fmt.Printf("health       self-monitoring + self-healing + self-governing ON — health every 30s\n")

	// PeerWarden enforces the Scorer's verdicts: it disconnects banned peers so a
	// persistent liar stops occupying a slot. Sweep cadence is the Governor's
	// lever. Only meaningful with p2p; nil seams => no-op.
	if peersProbe != nil && p2pDisconnect != nil {
		warden := &health.PeerWarden{
			Peers: peersProbe, Disconnect: p2pDisconnect, Log: log.Printf,
			Interval: func() time.Duration { return time.Duration(atomic.LoadInt64(&wardenIntervalNs)) },
			Poke:     wardenPoke,
		}
		go func() { _ = warden.Run(ctx) }()
		fmt.Printf("health       self-correcting ON — auto-disconnects banned peers (governed cadence)\n")
	}

	// Faucet: gives a new wallet enough LXS for its first gas to create a token.
	// Funded by a persistent faucet wallet the operator tops up.
	var faucetKey *crypto.PrivateKey
	var faucetAmount *big.Int
	if *faucet {
		fk, ephemeral, err := loadOrCreateKey(*dataDir, "faucet.key")
		if err != nil {
			return err
		}
		faucetKey = fk
		faucetAmount = new(big.Int).SetUint64(*faucetAmountWei)
		fmt.Printf("faucet       ON — /faucet gives %d wei/claim to new addresses (first gas)\n", *faucetAmountWei)
		fmt.Printf("faucet       FUND this wallet with LXS so it can dispense: %s\n", fk.Address().Hex())
		if ephemeral {
			fmt.Printf("faucet       WARNING faucet wallet is EPHEMERAL (no -datadir) — key + funds lost on restart\n")
		}
	}

	peerCount := func() int {
		if peersProbe != nil {
			return len(peersProbe())
		}
		return 0
	}

	n := node.New(node.Config{
		RPCAddr:        *rpcAddr,
		BlockTime:      *blockTime,
		Mine:           *mine,
		EmptyBlocks:    *emptyBlocks,
		TxBroadcaster:  txBroadcast,
		RPCAPIKeys:     apiKeys,
		RPCCORSOrigins: corsOrigins,
		FaucetKey:      faucetKey,
		FaucetAmount:   faucetAmount,
		PeerCount:      peerCount,
	}, bc, pool, prod)

	return n.Run(ctx)
}

// loadOrCreateKey returns a persistent key from <dataDir>/<filename>, generating
// and saving it (0600) on first run. With no dataDir there is nowhere durable to
// keep it, so it returns an ephemeral key (the bool reports this).
func loadOrCreateKey(dataDir, filename string) (*crypto.PrivateKey, bool, error) {
	if dataDir == "" {
		k, err := crypto.GenerateKey()
		return k, true, err
	}
	path := dataDir + "/" + filename
	if b, err := os.ReadFile(path); err == nil {
		k, err := crypto.PrivateKeyFromHex(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, false, fmt.Errorf("reading key %s: %w", path, err)
		}
		return k, false, nil
	}
	k, err := crypto.GenerateKey()
	if err != nil {
		return nil, false, err
	}
	if err := os.WriteFile(path, []byte(k.Hex()+"\n"), 0o600); err != nil {
		return nil, false, fmt.Errorf("saving key %s: %w", path, err)
	}
	return k, false, nil
}

func balance(args []string) error {
	fs := flag.NewFlagSet("balance", flag.ExitOnError)
	url := fs.String("rpc", defaultRPC, "node rpc url")
	addrHex := fs.String("addr", "", "account address")
	fs.Parse(args)
	if *addrHex == "" {
		return errors.New("-addr is required")
	}
	addr, err := common.AddressFromHex(*addrHex)
	if err != nil {
		return err
	}
	c := rpc.NewClient(*url)
	var bal rpc.Quantity
	if err := c.Call("chain_getBalance", &bal, addr); err != nil {
		return err
	}
	var nonce rpc.Quantity
	if err := c.Call("chain_getNonce", &nonce, addr); err != nil {
		return err
	}
	fmt.Printf("address  %s\n", addr.Hex())
	fmt.Printf("balance  %s\n", bal.Int)
	fmt.Printf("nonce    %d\n", nonce.U64())
	return nil
}

// send signs locally and submits the signed bytes. The private key never leaves
// this process: the node is a validator and relay, not a custodian.
func send(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	url := fs.String("rpc", defaultRPC, "node rpc url")
	keyHex := fs.String("key", "", "sender private key")
	toHex := fs.String("to", "", "recipient address (omit to DEPLOY a contract)")
	dataHex := fs.String("data", "", "hex call data, or init code when deploying")
	gasLimit := fs.Uint64("gas", types.IntrinsicGas, "gas limit (raise it for contract calls/deploys)")
	valueStr := fs.String("value", "0", "amount to send")
	gasPriceStr := fs.String("gas-price", "1", "gas price")
	nonceOverride := fs.Int64("nonce", -1, "override nonce (default: query the node)")
	wait := fs.Bool("wait", false, "wait for the tx to be mined")
	fs.Parse(args)

	if *keyHex == "" {
		return errors.New("-key is required")
	}
	if *toHex == "" && *dataHex == "" {
		return errors.New("nothing to do: give -to (transfer/call) or -data (deploy)")
	}
	key, err := crypto.PrivateKeyFromHex(*keyHex)
	if err != nil {
		return err
	}
	// An empty -to is a deployment: To stays nil and the VM treats the data as
	// init code whose return value becomes the contract's runtime.
	var toPtr *common.Address
	if *toHex != "" {
		to, err := common.AddressFromHex(*toHex)
		if err != nil {
			return err
		}
		toPtr = &to
	}
	data, err := parseHexArg(*dataHex)
	if err != nil {
		return fmt.Errorf("bad -data: %w", err)
	}
	value, ok := new(big.Int).SetString(*valueStr, 10)
	if !ok {
		return fmt.Errorf("bad value %q", *valueStr)
	}
	gasPrice, ok := new(big.Int).SetString(*gasPriceStr, 10)
	if !ok {
		return fmt.Errorf("bad gas price %q", *gasPriceStr)
	}

	c := rpc.NewClient(*url)

	var chainID rpc.Quantity
	if err := c.Call("chain_chainId", &chainID); err != nil {
		return err
	}

	nonce := uint64(0)
	if *nonceOverride >= 0 {
		nonce = uint64(*nonceOverride)
	} else {
		var n rpc.Quantity
		if err := c.Call("chain_getNonce", &n, key.Address()); err != nil {
			return err
		}
		nonce = n.U64()
	}

	tx := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID.U64(), Nonce: nonce,
		To: toPtr, Value: value, GasLimit: *gasLimit, GasPrice: gasPrice, Data: data,
	}
	if err := tx.Sign(key); err != nil {
		return err
	}

	var hash common.Hash
	if err := c.Call("chain_sendTransaction", &hash, rpc.FromTx(tx)); err != nil {
		return err
	}
	fmt.Printf("from   %s\n", key.Address().Hex())
	fmt.Printf("nonce  %d\n", nonce)
	fmt.Printf("tx     %s\n", hash.Hex())

	if !*wait {
		return nil
	}
	fmt.Printf("waiting...\n")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var r rpc.ReceiptResult
		err := c.Call("chain_getTransactionReceipt", &r, hash)
		if err == nil {
			fmt.Printf("mined in block %d  status %d  gas %d\n",
				r.BlockHeight.U64(), r.Status.U64(), r.GasUsed.U64())
			if r.ContractAddress != nil {
				fmt.Printf("contract %s\n", r.ContractAddress.Hex())
			}
			for i, l := range r.Logs {
				fmt.Printf("log[%d] %s topics=%d data=%d bytes\n", i, l.Address.Hex(), len(l.Topics), len(l.Data))
			}
			return nil
		}
		if !errors.Is(err, rpc.ErrNullResult) {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.New("timed out waiting for the tx to be mined")
}

// createToken deploys a fixed-supply ERC-20 in one command: builds the UserToken
// deploy bytecode for the given name/symbol/supply, signs locally, submits it,
// and prints the token address (derivable from sender+nonce before it is mined).
//
//	lxs create-token -key HEX -name "My Coin" -symbol MYC -supply 1000000
//
// validateTokenMeta bounds a token's name/symbol to printable ASCII within sane
// lengths, with no leading/trailing whitespace. Avalanche enforces the same as
// consensus rules; on a launchpad it blunts lookalike/oversized-metadata phishing.
func validateTokenMeta(name, symbol string) error {
	check := func(field, s string, max int) error {
		if len(s) == 0 || len(s) > max {
			return fmt.Errorf("%s must be 1..%d bytes", field, max)
		}
		for i := 0; i < len(s); i++ {
			if s[i] < 0x20 || s[i] > 0x7e {
				return fmt.Errorf("%s must be printable ASCII", field)
			}
		}
		if strings.TrimSpace(s) != s {
			return fmt.Errorf("%s must not have leading or trailing whitespace", field)
		}
		return nil
	}
	if err := check("name", name, 64); err != nil {
		return err
	}
	return check("symbol", symbol, 12)
}

// launchCoin creates a bonding-curve coin on the launchpad factory straight from the
// binary — no website, no operator server. As long as the chain runs (its miners), the
// PumpFactory contract lives on-chain and anyone can call it this way, so the ability to
// launch (and trade) coins survives independently of any hosted frontend.
func launchCoin(args []string) error {
	fs := flag.NewFlagSet("launch-coin", flag.ExitOnError)
	url := fs.String("rpc", defaultRPC, "node rpc url")
	keyHex := fs.String("key", "", "your private key (signs locally, never sent over RPC)")
	factoryHex := fs.String("factory", "", "the PumpFactory (launchpad) address on this chain")
	name := fs.String("name", "", `coin name, e.g. "Doge LXS"`)
	symbol := fs.String("symbol", "", "coin symbol, e.g. DOGE")
	imagePath := fs.String("image", "", "optional path to a small (<=12 KB) thumbnail carried in the Created event")
	buyStr := fs.String("buy", "0", "optional first buy in whole LXS, bought to you atomically in the same tx (anti-snipe)")
	gasLimit := fs.Uint64("gas", 2_000_000, "gas limit")
	wait := fs.Bool("wait", true, "wait for it to be mined")
	fs.Parse(args)

	if *keyHex == "" || *factoryHex == "" || *name == "" || *symbol == "" {
		return errors.New("need -key, -factory, -name and -symbol")
	}
	if err := validateTokenMeta(*name, *symbol); err != nil {
		return err
	}
	key, err := crypto.PrivateKeyFromHex(*keyHex)
	if err != nil {
		return err
	}
	factory, err := common.AddressFromHex(*factoryHex)
	if err != nil {
		return fmt.Errorf("bad -factory: %w", err)
	}
	var image []byte
	if *imagePath != "" {
		image, err = os.ReadFile(*imagePath)
		if err != nil {
			return fmt.Errorf("read -image: %w", err)
		}
		if len(image) > 12288 {
			return fmt.Errorf("image is %d bytes; the on-chain cap is 12288 — use a smaller thumbnail", len(image))
		}
	}
	buyWhole, ok := new(big.Int).SetString(*buyStr, 10)
	if !ok || buyWhole.Sign() < 0 {
		return fmt.Errorf("bad -buy %q", *buyStr)
	}
	value := new(big.Int).Mul(buyWhole, big.NewInt(1e18))
	data := contracts.PumpCreateCalldata(*name, *symbol, image, big.NewInt(0))

	c := rpc.NewClient(*url)
	var chainID rpc.Quantity
	if err := c.Call("chain_chainId", &chainID); err != nil {
		return err
	}
	var n rpc.Quantity
	if err := c.Call("chain_getNonce", &n, key.Address()); err != nil {
		return err
	}
	tx := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID.U64(), Nonce: n.U64(),
		To: &factory, Value: value, GasLimit: *gasLimit, GasPrice: big.NewInt(1), Data: data,
	}
	if err := tx.Sign(key); err != nil {
		return err
	}
	var hash common.Hash
	if err := c.Call("chain_sendTransaction", &hash, rpc.FromTx(tx)); err != nil {
		return err
	}
	fmt.Printf("launching coin %q (%s) on factory %s by %s\n", *name, *symbol, factory.Hex(), key.Address().Hex())
	if value.Sign() > 0 {
		fmt.Printf("first buy: %s LXS, bought to you atomically\n", buyWhole)
	}
	fmt.Printf("tx     %s\n", hash.Hex())

	if !*wait {
		return nil
	}
	fmt.Printf("waiting for it to be mined...\n")
	topic := contracts.PumpCreatedTopic()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var r rpc.ReceiptResult
		err := c.Call("chain_getTransactionReceipt", &r, hash)
		if err == nil {
			if r.Status.U64() != 1 {
				return fmt.Errorf("create reverted (status %d) — raise -gas?", r.Status.U64())
			}
			var coin common.Address
			for _, l := range r.Logs {
				if len(l.Topics) > 0 && l.Topics[0] == topic && len(l.Data) >= 32 {
					copy(coin[:], l.Data[12:32])
				}
			}
			fmt.Printf("DONE — %s is live on the curve at %s (block %d). Anyone can buy/sell it with LXS.\n",
				*symbol, coin.Hex(), r.BlockHeight.U64())
			return nil
		}
		if !errors.Is(err, rpc.ErrNullResult) {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.New("timed out waiting for the create to be mined")
}

func createToken(args []string) error {
	fs := flag.NewFlagSet("create-token", flag.ExitOnError)
	url := fs.String("rpc", defaultRPC, "node rpc url")
	keyHex := fs.String("key", "", "your private key (signs locally, never sent over RPC)")
	name := fs.String("name", "", `token name, e.g. "My Coin"`)
	symbol := fs.String("symbol", "", "token symbol, e.g. MYC")
	supplyStr := fs.String("supply", "1000000", "total supply in whole tokens (all minted to you)")
	gasLimit := fs.Uint64("gas", 800_000, "gas limit for the deploy")
	wait := fs.Bool("wait", true, "wait for the deploy to be mined")
	fs.Parse(args)

	if *keyHex == "" || *name == "" || *symbol == "" {
		return errors.New("need -key, -name and -symbol")
	}
	if err := validateTokenMeta(*name, *symbol); err != nil {
		return err
	}
	key, err := crypto.PrivateKeyFromHex(*keyHex)
	if err != nil {
		return err
	}
	supply, ok := new(big.Int).SetString(*supplyStr, 10)
	if !ok || supply.Sign() <= 0 {
		return fmt.Errorf("bad supply %q", *supplyStr)
	}
	deploy := contracts.UserTokenDeploy(*name, *symbol, new(big.Int).Mul(supply, big.NewInt(1e18)))

	c := rpc.NewClient(*url)
	var chainID rpc.Quantity
	if err := c.Call("chain_chainId", &chainID); err != nil {
		return err
	}
	var n rpc.Quantity
	if err := c.Call("chain_getNonce", &n, key.Address()); err != nil {
		return err
	}
	nonce := n.U64()

	tx := &types.Transaction{
		Type: types.TxTypeTransfer, ChainID: chainID.U64(), Nonce: nonce,
		To: nil, Value: big.NewInt(0), GasLimit: *gasLimit, GasPrice: big.NewInt(1), Data: deploy,
	}
	if err := tx.Sign(key); err != nil {
		return err
	}
	tokenAddr := state.CreateAddress(key.Address(), nonce)

	var hash common.Hash
	if err := c.Call("chain_sendTransaction", &hash, rpc.FromTx(tx)); err != nil {
		return err
	}
	fmt.Printf("creating token %q (%s) — supply %s, owner %s\n", *name, *symbol, supply, key.Address().Hex())
	fmt.Printf("tx     %s\n", hash.Hex())
	fmt.Printf("token  %s\n", tokenAddr.Hex())

	if !*wait {
		return nil
	}
	fmt.Printf("waiting for it to be mined...\n")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var r rpc.ReceiptResult
		err := c.Call("chain_getTransactionReceipt", &r, hash)
		if err == nil {
			if r.Status.U64() != 1 {
				return fmt.Errorf("deploy reverted (status %d) — raise -gas?", r.Status.U64())
			}
			fmt.Printf("DONE — %s is live at %s (block %d). You hold %s %s.\n",
				*symbol, tokenAddr.Hex(), r.BlockHeight.U64(), supply, *symbol)
			return nil
		}
		if !errors.Is(err, rpc.ErrNullResult) {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.New("timed out waiting for the deploy to be mined")
}

// callContract runs a read-only contract call (chain_call) and prints the hex
// return data. No key, no transaction, nothing committed — reads a view like
// balanceOf without a block.
func callContract(args []string) error {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	url := fs.String("rpc", defaultRPC, "node rpc url")
	toHex := fs.String("to", "", "contract address")
	dataHex := fs.String("data", "", "hex call data")
	fromHex := fs.String("from", "", "caller address (optional)")
	fs.Parse(args)

	if *toHex == "" {
		return errors.New("-to is required")
	}
	to, err := common.AddressFromHex(*toHex)
	if err != nil {
		return err
	}
	data, err := parseHexArg(*dataHex)
	if err != nil {
		return fmt.Errorf("bad -data: %w", err)
	}
	args2 := rpc.CallArgs{To: to, Data: data}
	if *fromHex != "" {
		from, err := common.AddressFromHex(*fromHex)
		if err != nil {
			return err
		}
		args2.From = &from
	}

	var out rpc.Data
	if err := rpc.NewClient(*url).Call("chain_call", &out, args2); err != nil {
		return err
	}
	fmt.Printf("0x%x\n", []byte(out))
	return nil
}

// parseHexArg decodes an optional 0x-prefixed hex string; "" means empty bytes.
func parseHexArg(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return nil, nil
	}
	return hex.DecodeString(s)
}

func getBlock(args []string) error {
	fs := flag.NewFlagSet("block", flag.ExitOnError)
	url := fs.String("rpc", defaultRPC, "node rpc url")
	height := fs.Int64("n", -1, "block height (default: head)")
	hashHex := fs.String("hash", "", "block hash")
	full := fs.Bool("full", false, "include full transactions")
	fs.Parse(args)

	c := rpc.NewClient(*url)
	var out json.RawMessage

	switch {
	case *hashHex != "":
		h, err := common.HashFromHex(*hashHex)
		if err != nil {
			return err
		}
		err = c.Call("chain_getBlockByHash", &out, h, *full)
		if errors.Is(err, rpc.ErrNullResult) {
			return errors.New("no such block")
		} else if err != nil {
			return err
		}
	default:
		n := uint64(0)
		if *height >= 0 {
			n = uint64(*height)
		} else {
			var head rpc.Quantity
			if err := c.Call("chain_blockNumber", &head); err != nil {
				return err
			}
			n = head.U64()
		}
		err := c.Call("chain_getBlockByNumber", &out, rpc.QU(n), *full)
		if errors.Is(err, rpc.ErrNullResult) {
			return errors.New("no such block")
		} else if err != nil {
			return err
		}
	}

	var pretty map[string]interface{}
	if err := json.Unmarshal(out, &pretty); err != nil {
		return err
	}
	enc, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(enc))
	return nil
}

func getReceipt(args []string) error {
	fs := flag.NewFlagSet("receipt", flag.ExitOnError)
	url := fs.String("rpc", defaultRPC, "node rpc url")
	txHex := fs.String("tx", "", "transaction hash")
	fs.Parse(args)
	if *txHex == "" {
		return errors.New("-tx is required")
	}
	h, err := common.HashFromHex(*txHex)
	if err != nil {
		return err
	}
	c := rpc.NewClient(*url)
	var out json.RawMessage
	err = c.Call("chain_getTransactionReceipt", &out, h)
	if errors.Is(err, rpc.ErrNullResult) {
		return errors.New("no receipt: the tx is unknown or still pending")
	} else if err != nil {
		return err
	}
	var pretty map[string]interface{}
	json.Unmarshal(out, &pretty)
	enc, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(enc))
	return nil
}

func demo() error {
	alice, _ := crypto.GenerateKey()
	bob, _ := crypto.GenerateKey()
	miner, _ := crypto.GenerateKey()

	g := &core.Genesis{
		Name:      common.NetworkName,
		ChainID:   1337,
		Timestamp: 1700000000000,
		GasLimit:  30_000_000,
		Alloc: map[common.Address]*core.BigStr{
			alice.Address(): {Int: big.NewInt(1_000_000)},
		},
	}

	bc := core.NewMemBlockchain(g)
	pool := mempool.New(4096)
	prod := core.NewProducer(bc, pool, miner.Address())

	fmt.Printf("genesis      %s\n", bc.Head().Hash().Hex())
	fmt.Printf("alice        %s  balance %s\n\n", alice.Address().Hex(), bc.StateSnapshot().Balance(alice.Address()))

	for i := 0; i < 3; i++ {
		tx := types.NewTransaction(1337, uint64(i), bob.Address(), big.NewInt(1000), types.IntrinsicGas, big.NewInt(int64(i+1)), nil)
		if err := tx.Sign(alice); err != nil {
			return err
		}
		if err := pool.Add(tx, 1337); err != nil {
			return err
		}
		blk, err := prod.Seal()
		if err != nil {
			return err
		}
		fmt.Printf("block %d      %s\n", blk.Height(), blk.Hash().Hex())
		fmt.Printf("  txs %d  gas %d  receipt root %s\n", len(blk.Txs), blk.Header.GasUsed, blk.Header.ReceiptRoot.Hex())
	}

	s := bc.StateSnapshot()
	fmt.Printf("\nfinal state\n")
	fmt.Printf("  alice  balance %-10s nonce %d\n", s.Balance(alice.Address()), s.Nonce(alice.Address()))
	fmt.Printf("  bob    balance %-10s nonce %d\n", s.Balance(bob.Address()), s.Nonce(bob.Address()))
	fmt.Printf("  miner  balance %-10s (fees)\n", s.Balance(miner.Address()))

	total := new(big.Int)
	for _, acc := range s.Accounts() {
		total.Add(total, acc.Balance)
	}
	fmt.Printf("\n  supply %s (genesis was 1000000 — must match)\n", total)

	fmt.Printf("\nadversarial check: block with a forged state root\n")
	blk, _ := prod.Build()
	blk.Header.StateRoot = common.Hash{0xde, 0xad}
	if err := bc.InsertBlock(&types.Block{Header: blk.Header, Txs: blk.Txs}); err != nil {
		fmt.Printf("  rejected: %v\n", err)
	} else {
		fmt.Printf("  ACCEPTED — this is a critical bug\n")
	}
	return nil
}
