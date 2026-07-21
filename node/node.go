package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sync"
	"time"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/rpc"
	"lxs/state"
	"lxs/types"
)

// FaucetRateLimit throttles the /faucet endpoint per client IP, far stricter than
// the RPC limiter: a new user claims once (one OPTIONS + one POST), so a small
// burst with a slow refill passes every honest claim while capping an abuser to a
// trickle. With one-claim-per-address and the finite wallet balance, this bounds
// abuse.
var FaucetRateLimit = rpc.RateLimit{Burst: 6, PerSecond: 0.5, MaxTracked: 8192}

type Config struct {
	RPCAddr   string
	BlockTime time.Duration
	// Mine controls whether this node produces blocks. Default false: a node that
	// mines because nobody told it not to forks a devnet by accident.
	Mine        bool
	EmptyBlocks bool

	// TxBroadcaster, if set, gossips a locally submitted transaction to the
	// network. nil on a node with no p2p — the tx still enters the local pool.
	TxBroadcaster func(*types.Transaction) error

	// RPCAPIKeys, if non-empty, enables API-key auth on the RPC port: every request
	// must present one as `Authorization: Bearer <key>`. Empty (the default) leaves
	// the port open and logs a loud warning.
	RPCAPIKeys []string

	// RPCCORSOrigins is the browser cross-origin allowlist. Empty (the default)
	// grants no web origin — the safe default. A single "*" allows any origin.
	RPCCORSOrigins []string

	// FaucetKey, if set, enables a /faucet endpoint that dispenses FaucetAmount wei
	// to a new address so it can pay its first gas. Funded by this key's own wallet.
	// nil = off.
	FaucetKey    *crypto.PrivateKey
	FaucetAmount *big.Int

	// PeerCount, if set, reports the number of connected p2p peers for the mining
	// dashboard's stats. A hook, not a p2p import (rpc/node must not depend on p2p).
	PeerCount func() int

	// PoolHandler, if set, mounts the mining-pool API under /pool/. An http.Handler
	// hook rather than a pool import for the same reason PeerCount is: node must
	// not depend on the pool engine.
	PoolHandler http.Handler
}

// PoolRateLimit throttles /pool/* per client IP. A worker legitimately polls
// work every few seconds and posts a share every few more; this cap passes tens
// of workers behind one NAT while stopping a share-flood from one host.
var PoolRateLimit = rpc.RateLimit{Burst: 60, PerSecond: 10, MaxTracked: 8192}

// MiningStats gathers the numbers the mining dashboard shows in one snapshot. hashCount is
// cumulative (the dashboard derives a hashrate from the delta between polls); balance is the
// real earnings, and rewardWei lets the UI show "blocks won x reward" honestly.
func MiningStats(bc *core.Blockchain, prod *core.Producer, mining bool, peers int) map[string]any {
	var height, diff uint64
	if head := bc.Head(); head != nil {
		height = head.Header.Height
		diff = head.Header.Difficulty
	}
	cb := prod.Coinbase()
	return map[string]any{
		"mining":     mining,
		"coinbase":   cb.Hex(),
		"balance":    bc.BalanceAt(cb).String(),
		"height":     height,
		"difficulty": diff,
		"blocksWon":  prod.BlocksWon(),
		"hashCount":  core.HashCount(),
		"peers":      peers,
		"rewardWei":  state.BlockRewardAt(height).String(),
	}
}

type Node struct {
	cfg  Config
	bc   *core.Blockchain
	pool *mempool.Mempool
	prod *core.Producer
	srv  *http.Server
}

func New(cfg Config, bc *core.Blockchain, pool *mempool.Mempool, prod *core.Producer) *Node {
	server := rpc.NewServer()
	api := rpc.NewAPI(bc, pool)
	if cfg.TxBroadcaster != nil {
		api.SetTxBroadcaster(cfg.TxBroadcaster)
	}
	api.Register(server)

	// chain_miningStats powers the mining dashboard: one call with everything a miner
	// wants to see. Registered here rather than in the pure rpc.API because it needs the
	// producer (coinbase, blocks won) and a peer count, which the node has.
	server.Register("chain_miningStats", func(json.RawMessage) (interface{}, error) {
		peers := 0
		if cfg.PeerCount != nil {
			peers = cfg.PeerCount()
		}
		return MiningStats(bc, prod, cfg.Mine, peers), nil
	})

	mux := http.NewServeMux()
	mux.Handle("/", server)

	// Middleware order, outermost first: rate-limit -> CORS -> auth -> mux.
	// Rate-limit is outermost as the cheapest rejection (a token debit, no crypto).
	// CORS is next because a browser preflight (OPTIONS) carries no credentials and
	// must be answered before auth, which would 401 it. Auth sits last, turning away
	// unauthenticated requests before they reach any method.
	var handler http.Handler = rpc.NewRateLimiter(
		rpc.NewCORS(
			rpc.NewAuth(mux, cfg.RPCAPIKeys),
			cfg.RPCCORSOrigins,
		),
		rpc.DefaultRateLimit, nil,
	)

	// The faucet (if on) is routed outside the RPC auth/CORS middleware: a public
	// giveaway with its own permissive CORS, it must work from the website even when
	// the RPC port is locked with API-key auth or a strict CORS allowlist.
	if cfg.FaucetKey != nil {
		amount := cfg.FaucetAmount
		if amount == nil || amount.Sign() <= 0 {
			amount = new(big.Int).Set(common.OneLXS) // default: 1 LXS, enough to create a token
		}
		faucet := NewFaucet(cfg.FaucetKey, amount, bc, pool, cfg.TxBroadcaster)
		// It dispenses real value, so it gets its own strict per-IP limiter. The RPC
		// limiter above does not cover it (mounted outside that chain), so without
		// this a single client mints unlimited addresses, floods the mempool, and
		// drains the wallet as fast as it can POST.
		outer := http.NewServeMux()
		outer.Handle("/faucet", rpc.NewRateLimiter(faucet, FaucetRateLimit, nil))
		outer.Handle("/", handler)
		handler = outer
	}

	// The pool API mounts outside RPC auth/CORS for the same reason the faucet
	// does: workers and the website must reach it even on a locked-down RPC. It
	// gets its own limiter — share traffic has a different honest profile than
	// RPC reads.
	if cfg.PoolHandler != nil {
		outer := http.NewServeMux()
		outer.Handle("/pool/", rpc.NewRateLimiter(cfg.PoolHandler, PoolRateLimit, nil))
		outer.Handle("/", handler)
		handler = outer
	}

	return &Node{
		cfg:  cfg,
		bc:   bc,
		pool: pool,
		prod: prod,
		srv: &http.Server{
			Addr:    cfg.RPCAddr,
			Handler: handler,
			// Timeouts are not optional on a public listener: without them a
			// single slow client holds a connection open forever, exhausting
			// file descriptors.
			ReadTimeout:       5 * time.Second,
			ReadHeaderTimeout: 2 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
		},
	}
}

// Run blocks until ctx is cancelled.
func (n *Node) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	var wg sync.WaitGroup

	go func() {
		log.Printf("rpc listening on http://%s", n.cfg.RPCAddr)
		if err := n.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	// A follower that does not produce is a normal node. The devnet runs one
	// producer and N followers so the only question is whether blocks arrive and
	// everyone agrees; with real PoW, more producers is valid but less legible.
	if n.cfg.Mine {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.produce(ctx)
		}()
	} else {
		log.Printf("follower mode: not producing blocks")
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		// Give in-flight RPC calls a moment to finish rather than cutting them off
		// mid-response.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		log.Printf("shutting down")
		err := n.srv.Shutdown(shutdownCtx)
		// Wait (bounded) for the producer to observe cancellation and stop sealing
		// before returning, so the caller's chain Close does not race a block being
		// committed. Bounded because a real seal can take a while; a block either
		// commits atomically or fails cleanly, so abandoning one is safe.
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			log.Printf("shutdown: producer still sealing, proceeding")
		}
		return err
	}
}

// produce mines continuously. There is no timer: the work is the clock. Seal
// grinds a nonce until the header beats its target (about one TargetBlockTime on
// tuned difficulty), then the loop starts the next block. Difficulty adjustment,
// not a ticker, keeps the cadence honest.
//
// The loop is contestable: with real work behind each block, more than one node
// can mine and the heaviest chain resolves the race. The devnet runs one miner
// for legibility; the loop does not care how many there are.
func (n *Node) produce(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		// Skipping empty blocks keeps a devnet log readable. A real chain cannot:
		// with no blocks there is no heartbeat and peers cannot tell quiet from
		// dead. When there is nothing to mine, wait rather than spin the CPU.
		if !n.cfg.EmptyBlocks && n.pool.Len() == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}

		blk, err := n.prod.Build() // grinds the nonce, does not commit yet
		if err != nil {
			if core.IsMiningAborted(err) {
				continue // a competing block won the race; rebuild on the new head
			}
			log.Printf("build failed: %v", err)
			continue
		}
		// A non-empty pool does not guarantee an includable tx: a stuck
		// nonce-gap tx sits forever and every candidate comes out empty. Without
		// this check the loop seals an unbounded run of empty blocks. Drop the
		// candidate and back off.
		if !n.cfg.EmptyBlocks && len(blk.Txs) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if err := n.prod.Commit(blk); err != nil {
			log.Printf("commit failed: %v", err)
			continue
		}
		log.Printf("mined block %d  %s  txs=%d  gas=%d  difficulty=%d",
			blk.Height(), short(blk.Hash().Hex()), len(blk.Txs), blk.Header.GasUsed, blk.Header.Difficulty)
	}
}

func short(s string) string {
	if len(s) < 12 {
		return s
	}
	return fmt.Sprintf("%s..%s", s[:8], s[len(s)-4:])
}
