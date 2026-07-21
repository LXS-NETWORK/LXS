package main

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"time"

	"lxs/core"
	"lxs/node"
	"lxs/web"
)

// serveDashboard serves the embedded mining dashboard plus a same-origin GET /stats endpoint
// that returns node.MiningStats as JSON. Same-origin means the page reads live stats with a
// plain fetch and no CORS setup — the miner just opens the address and watches. Returns the
// running server so the caller closes it on shutdown.
func serveDashboard(addr string, bc *core.Blockchain, prod *core.Producer, mining bool, peerCount func() int) (*http.Server, error) {
	sub, err := fs.Sub(web.Dashboard, "dashboard")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		peers := 0
		if peerCount != nil {
			peers = peerCount()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(node.MiningStats(bc, prod, mining, peers))
	})
	mux.Handle("/", files)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("mining dashboard server: %v", err)
		}
	}()
	return srv, nil
}
