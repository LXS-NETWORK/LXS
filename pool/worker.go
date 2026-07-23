package pool

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"lxs/core"
	"lxs/types"
)

// Worker is the pool-mining client: fetch work, grind, submit shares. It runs NO
// node and holds NO chain state — that is the point: a weak machine contributes
// hashes without syncing anything, and the pool address is where its earnings
// accrue. It hashes with the same core.Grind the chain validates with.
type Worker struct {
	BaseURL  string // pool base, e.g. https://lxsnetwork.duckdns.org
	Coinbase string // payout address (0x…) — sent with every share
	Logf     func(string, ...any)

	client http.Client
}

type workResp struct {
	WorkID      string          `json:"workId"`
	Header      json.RawMessage `json:"header"`
	ShareTarget string          `json:"shareTarget"`
	ShareDiff   uint64          `json:"shareDiff"`
	Difficulty  uint64          `json:"difficulty"`
	Height      uint64          `json:"height"`
}

// grindSlice bounds one uninterrupted grinding burst. Between slices the worker
// re-checks the pool for fresh work, so it never wastes more than this long on a
// template obsoleted by a new block.
const grindSlice = 4 * time.Second

// Run mines against the pool until ctx is cancelled. Network errors back off and
// retry forever — a pool restart must not kill every worker attached to it.
func (w *Worker) Run(ctx context.Context) error {
	if w.Logf == nil {
		w.Logf = func(string, ...any) {}
	}
	w.client.Timeout = 10 * time.Second
	w.BaseURL = strings.TrimRight(w.BaseURL, "/")

	var (
		cur       workResp
		hdr       *types.Header
		target    *big.Int
		next      uint64
		shares    uint64
		hashes    uint64
		lastStats = time.Now()
	)

	for ctx.Err() == nil {
		fresh, err := w.fetchWork(ctx)
		if err != nil {
			w.Logf("pool unreachable (%v) — retrying in 5s", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return nil
			}
			continue
		}
		if fresh.WorkID != cur.WorkID {
			// New template: parse the header fresh and restart from a random
			// nonce. Random start is what keeps two workers on the same
			// template from hashing the same range (see core.Grind).
			var h types.Header
			if err := json.Unmarshal(fresh.Header, &h); err != nil {
				w.Logf("bad work from pool: %v", err)
				if !sleepCtx(ctx, 5*time.Second) {
					return nil
				}
				continue
			}
			t, ok := new(big.Int).SetString(strings.TrimPrefix(fresh.ShareTarget, "0x"), 16)
			if !ok {
				w.Logf("bad share target from pool: %q", fresh.ShareTarget)
				if !sleepCtx(ctx, 5*time.Second) {
					return nil
				}
				continue
			}
			cur, hdr, target, next = fresh, &h, t, randNonce()
		}

		// Grind one slice. stop fires at the slice deadline; a found share
		// returns early with its nonce and we resume right after it. A sync.Once
		// guards the close: if a share is found at the same instant the deadline
		// fires, the timer's func and the cleanup below would otherwise both
		// close(stop) — a "close of closed channel" panic that kills the miner.
		stop := make(chan struct{})
		var once sync.Once
		closeStop := func() { once.Do(func() { close(stop) }) }
		timer := time.AfterFunc(grindSlice, closeStop)
		start := next
		nonce, found := core.Grind(hdr, target, next, stop)
		timer.Stop()
		closeStop()
		hashes += nonce - start
		next = nonce
		if found {
			next = nonce + 1
			isBlock, err := w.submitShare(ctx, cur.WorkID, nonce)
			switch {
			case err != nil:
				w.Logf("share rejected: %v", err)
			case isBlock:
				shares++
				w.Logf("share accepted (#%d) — 🎉 the POOL WON block %d! Your cut pays out after it matures.", shares, cur.Height)
			default:
				shares++
				w.Logf("share accepted (#%d) at height %d", shares, cur.Height)
			}
		}
		if time.Since(lastStats) >= 8*time.Second {
			secs := time.Since(lastStats).Seconds()
			w.Logf("hashrate ~%.0f H/s · shares %d · pool height %d · share difficulty %d",
				float64(hashes)/secs, shares, cur.Height, cur.ShareDiff)
			hashes, lastStats = 0, time.Now()
		}
	}
	return nil
}

func (w *Worker) fetchWork(ctx context.Context) (workResp, error) {
	var out workResp
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.BaseURL+"/pool/work", nil)
	if err != nil {
		return out, err
	}
	res, err := w.client.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("pool: %s", strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (w *Worker) submitShare(ctx context.Context, workID string, nonce uint64) (isBlock bool, err error) {
	payload := mustJSON(map[string]any{"workId": workID, "nonce": nonce, "address": w.Coinbase})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.BaseURL+"/pool/share", bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := w.client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	if err != nil {
		return false, err
	}
	if res.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	var out struct {
		Block bool `json:"block"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Block, nil
}

// randNonce draws a uniform 64-bit starting nonce from crypto/rand. Worker-side
// only — nothing consensus-critical, just collision avoidance between workers.
func randNonce() uint64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano()) // degraded but still spread out
	}
	return binary.LittleEndian.Uint64(b[:])
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
