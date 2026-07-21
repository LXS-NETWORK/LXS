package pool

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"lxs/common"
)

// The three share-rejection reasons a worker must tell apart: stale means
// refetch work (normal, every new block), duplicate and bad-share mean the
// worker (or an attacker) sent something worthless — no credit.
var (
	errStale     = errors.New("stale work — fetch new work")
	errDuplicate = errors.New("duplicate share")
	errBadShare  = errors.New("share does not meet the target")
)

// Handler serves the pool's public HTTP API. CORS is permissive like the
// faucet's: the website reads /pool/stats from the browser, and none of these
// endpoints carry credentials — a share IS its own proof of work, and the worker
// address only says where to pay.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/pool/work", s.serveWork)
	mux.HandleFunc("/pool/share", s.serveShare)
	mux.HandleFunc("/pool/stats", s.serveStats)
	return withCORS(mux)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) serveWork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	t := s.templates[s.current]
	var body []byte
	if t != nil {
		body = t.workJSON
	}
	s.mu.Unlock()
	if body == nil {
		http.Error(w, "no work yet — pool is starting", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (s *Server) serveShare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		WorkID  string `json:"workId"`
		Nonce   uint64 `json:"nonce"`
		Address string `json:"address"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	workID, err := common.HashFromHex(req.WorkID)
	if err != nil {
		http.Error(w, "bad workId", http.StatusBadRequest)
		return
	}
	addr, err := common.AddressFromHex(req.Address)
	if err != nil {
		http.Error(w, "bad address", http.StatusBadRequest)
		return
	}

	isBlock, err := s.handleShare(workID, req.Nonce, addr)
	switch {
	case errors.Is(err, errStale):
		http.Error(w, err.Error(), http.StatusGone)
		return
	case errors.Is(err, errDuplicate):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, errBadShare):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "block": isBlock})
}

// serveStats is the pool's public ledger snapshot — what the website and the
// monitor app show, and what lets workers audit that shares are being counted.
func (s *Server) serveStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	now := time.Now()
	miners := 0
	for _, at := range s.lastSeen {
		if now.Sub(at) < 10*time.Minute {
			miners++
		}
	}
	// Hashrate estimate: Σ share-difficulty in the last 2 minutes ÷ 120s. Each
	// accepted share represents ~diff hashes of expected work, so this is the
	// honest aggregate rate, not a worker self-report.
	var work uint64
	for _, rs := range s.recent {
		if now.Sub(rs.at) < 2*time.Minute {
			work += rs.diff
		}
	}
	var shareDiff, height uint64
	if t := s.templates[s.current]; t != nil {
		shareDiff = t.shareDiff
		height = t.hdr.Height
	}
	resp := map[string]any{
		"poolAddress":   s.key.Address().Hex(),
		"minersActive":  miners,
		"hashrate":      work / 120,
		"totalShares":   s.totalShares,
		"blocksFound":   s.totalBlocks,
		"blocksOrphan":  s.totalOrphans,
		"pendingBlocks": len(s.pending),
		"balancesOwed":  len(s.balances),
		"totalPaidWei":  s.totalPaid.String(),
		"shareDiff":     shareDiff,
		"nextHeight":    height,
		"feeBps":        s.cfg.FeeBps,
		"confirmations": s.cfg.Confirmations,
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
