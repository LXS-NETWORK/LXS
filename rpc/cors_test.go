package rpc

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func corsWith(origins []string) (*CORS, *bool) {
	reached := new(bool)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
	return NewCORS(next, origins), reached
}

func doCORS(c *CORS, method, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	return rec
}

// TestCORSAllowedOriginIsEchoed: a POST from an allowlisted origin gets that
// exact origin echoed back (not a blanket "*"), and reaches the handler.
func TestCORSAllowedOriginIsEchoed(t *testing.T) {
	c, reached := corsWith([]string{"https://app.example"})
	rec := doCORS(c, http.MethodPost, "https://app.example")

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("allowed origin ACAO = %q, want the echoed origin", got)
	}
	if !*reached {
		t.Fatal("an allowed POST must still reach the handler")
	}
}

// TestCORSDisallowedOriginGetsNoGrant: a POST from an origin not on the list
// gets no Access-Control-Allow-Origin, so the browser blocks the JS read. The
// request still reaches the handler (CORS is a browser-read policy, not server
// access control — a non-browser client would ignore the missing header).
func TestCORSDisallowedOriginGetsNoGrant(t *testing.T) {
	c, reached := corsWith([]string{"https://app.example"})
	rec := doCORS(c, http.MethodPost, "https://evil.example")

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin got ACAO = %q, want none", got)
	}
	if !*reached {
		t.Fatal("the handler still runs; CORS gates the browser read, not the call")
	}
}

// TestCORSPreflightIsTerminated: an OPTIONS preflight from an allowed origin is
// answered here (204 + the Allow-* headers) and must not fall through to the
// next handler — forwarding it would hit auth/405 and the browser would give up.
func TestCORSPreflightIsTerminated(t *testing.T) {
	c, reached := corsWith([]string{"https://app.example"})
	rec := doCORS(c, http.MethodOptions, "https://app.example")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if *reached {
		t.Fatal("preflight must be terminated by CORS, never forwarded to the handler")
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("preflight must advertise the allowed methods")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Fatal("preflight must allow Authorization, or the browser strips the Bearer key")
	}
}

// TestCORSNoOriginPassesThrough: a request with no Origin header (curl, another
// node) is untouched — no CORS headers, handler reached. CORS must never change
// the answer to a non-browser client.
func TestCORSNoOriginPassesThrough(t *testing.T) {
	c, reached := corsWith([]string{"https://app.example"})
	rec := doCORS(c, http.MethodPost, "")

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("no-Origin request got ACAO = %q, want none", got)
	}
	if !*reached {
		t.Fatal("a non-browser request must reach the handler unchanged")
	}
}

// TestCORSWildcardAllowsAny: an explicit "*" entry opts into allowing any
// origin — answered with "*". This is opt-in; the empty default allows nothing.
func TestCORSWildcardAllowsAny(t *testing.T) {
	c, _ := corsWith([]string{"*"})
	rec := doCORS(c, http.MethodPost, "https://anything.example")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("wildcard ACAO = %q, want *", got)
	}
}

// TestCORSDefaultDeniesEverything: the empty allowlist (the default) grants no
// origin — the safe default that keeps a random visited page from reading a
// user's node.
func TestCORSDefaultDeniesEverything(t *testing.T) {
	c, _ := corsWith(nil)
	rec := doCORS(c, http.MethodPost, "https://app.example")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("empty allowlist granted ACAO = %q, want none (deny by default)", got)
	}
}
