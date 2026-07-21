package main

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"lxs/web"
)

// serveLaunchpad serves the embedded launchpad UI, injecting a per-node config (RPC URL,
// chain id, factory address) into index.html so the SAME static files work on every node
// with no editing. This is what makes the frontend decentralized: any node operator can
// run `lxs node -launchpad` and offer the create/trade UI, so it survives without the
// original operator's website. Returns the running server so the caller closes it on exit.
func serveLaunchpad(addr, rpcURL, factory string, chainID uint64) (*http.Server, error) {
	sub, err := fs.Sub(web.Launchpad, "launchpad")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}
	// Inject the node's config before app.js runs; app.js merges window.LXS_CONFIG over its defaults.
	cfg := fmt.Sprintf(`<script>window.LXS_CONFIG={"RPC_URL":%q,"CHAIN_ID":%d,"FACTORY_ADDRESS":%q};</script>`,
		rpcURL, chainID, factory)
	page := []byte(strings.Replace(string(index), "</head>", cfg+"</head>", 1))
	files := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
			return
		}
		files.ServeHTTP(w, r)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("launchpad UI server: %v", err)
		}
	}()
	return srv, nil
}
