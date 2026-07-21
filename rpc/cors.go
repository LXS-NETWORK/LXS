package rpc

import (
	"net/http"
	"strings"
)

// CORS enforces a cross-origin policy for browser clients.
//
// CORS is enforced by the browser, not the server: curl, ethers.js, or another
// node ignore these headers, so this is not access control (auth.go is). It
// decides which web origins' JavaScript the browser lets read this node's
// responses. The safe default is an empty allowlist: emit no CORS headers and
// the browser blocks every cross-origin read. A default of "*" would let any
// visited page read the node. A single "*" entry is opt-in, never the default.
type CORS struct {
	next     http.Handler
	allowAll bool
	allowed  map[string]bool
}

// NewCORS wraps next with an origin allowlist. Empty (the default) means no
// cross-origin browser access at all. A single "*" entry allows any origin.
func NewCORS(next http.Handler, origins []string) *CORS {
	c := &CORS{next: next, allowed: make(map[string]bool)}
	for _, o := range origins {
		o = strings.TrimSpace(o)
		switch {
		case o == "":
			continue
		case o == "*":
			c.allowAll = true
		default:
			c.allowed[o] = true
		}
	}
	return c
}

func (c *CORS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	allow := c.allowOrigin(origin)

	if allow != "" {
		w.Header().Set("Access-Control-Allow-Origin", allow)
		// Vary: Origin — the allowed value depends on the request Origin, so a
		// shared cache must not replay one origin's response to another.
		w.Header().Add("Vary", "Origin")
	}

	// A preflight OPTIONS is sent before the real POST to ask whether the call
	// is allowed. It carries no credentials, so it is answered here and not
	// forwarded: passing it down would hit auth (401) and the POST-only handler
	// (405), and the browser would read either as denied.
	if r.Method == http.MethodOptions {
		if allow != "" {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			// Authorization must be listed or the browser strips the Bearer key
			// from the real request.
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	c.next.ServeHTTP(w, r)
}

// allowOrigin returns the value to echo in Access-Control-Allow-Origin, or ""
// for no grant. A request with no Origin header is not a browser call (curl,
// another node); it gets "" and passes through. CORS must never change the
// answer to a non-browser client.
func (c *CORS) allowOrigin(origin string) string {
	if origin == "" {
		return ""
	}
	if c.allowAll {
		return "*"
	}
	if c.allowed[origin] {
		return origin // echo the specific origin, never a blanket "*"
	}
	return ""
}
