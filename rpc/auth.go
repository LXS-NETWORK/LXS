package rpc

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// Auth is an optional API-key gate in front of the RPC handler.
//
// All-or-nothing, not method-level: every method is either a public read or an
// already-signed tx submission (the node holds no keys), so there is no
// privileged subset to gate. A per-method policy earns its place only once a
// privileged namespace (debug_, miner control) exists.
//
// Keys are compared in constant time against their SHA-256 digests. String ==
// returns on the first differing byte, leaking prefix length through timing;
// subtle.ConstantTimeCompare on raw keys still leaks key length (it needs
// equal-length inputs). Hashing both sides to a fixed 32 bytes closes both.
type Auth struct {
	next http.Handler
	// keyHashes holds the SHA-256 of each accepted key. Plural for rotation:
	// an overlap window where the new key works before the old one is retired.
	keyHashes [][32]byte
}

// NewAuth wraps next with API-key authentication. An empty key set disables
// auth and logs a loud warning at construction: the operator must see that the
// port is unauthenticated, not discover it after it is drained. Auth off is a
// valid devnet choice; auth off by accident is an incident.
func NewAuth(next http.Handler, keys []string) *Auth {
	a := &Auth{next: next}
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue // an empty entry is not a key; must not enable auth
		}
		a.keyHashes = append(a.keyHashes, sha256.Sum256([]byte(k)))
	}
	if len(a.keyHashes) == 0 {
		log.Printf("rpc: WARNING — API-key auth is DISABLED; the RPC port is open to anyone who can reach it")
	}
	return a
}

func (a *Auth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// No keys configured: auth is off (the warning already fired at startup).
	if len(a.keyHashes) == 0 {
		a.next.ServeHTTP(w, r)
		return
	}
	if !a.authorized(r) {
		writeUnauthorized(w)
		return
	}
	a.next.ServeHTTP(w, r)
}

// authorized reports whether the request presents an accepted key. The scan
// does not stop at the first match: it OR-accumulates results and checks the
// total at the end. Early return would leak which key matched through timing.
func (a *Auth) authorized(r *http.Request) bool {
	presented, ok := bearerToken(r)
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(presented))
	var matched int
	for i := range a.keyHashes {
		matched |= subtle.ConstantTimeCompare(got[:], a.keyHashes[i][:])
	}
	return matched == 1
}

// bearerToken pulls the key from `Authorization: Bearer <key>`. The scheme is
// matched case-insensitively (RFC 7235; clients send Bearer/bearer/BEARER).
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const scheme = "bearer "
	if len(h) < len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(h[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}

func writeUnauthorized(w http.ResponseWriter) {
	// WWW-Authenticate names the required scheme; the 401 spec requires it.
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// One response for both missing and wrong key: distinguishing them tells a
	// prober whether it guessed the format right.
	_ = json.NewEncoder(w).Encode(response{
		JSONRPC: "2.0",
		Error:   &rpcError{CodeServerError, "unauthorized"},
		ID:      json.RawMessage("null"),
	})
}
