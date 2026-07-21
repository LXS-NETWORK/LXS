package rpc

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// fireAuth sends one request through the auth middleware, optionally with a
// Bearer token, and returns the status the client saw plus whether the
// protected handler was reached.
func fireAuth(a *Auth, token string, sendHeader bool) (int, bool) {
	reached := false
	a.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if sendHeader {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec.Code, reached
}

// TestAuthDisabledWhenNoKeys: with no keys configured the gate is a pass-through
// (devnet default). The loud warning is not asserted here; the open behaviour is
// the contract.
func TestAuthDisabledWhenNoKeys(t *testing.T) {
	a := NewAuth(nil, nil)
	if len(a.keyHashes) != 0 {
		t.Fatalf("no keys should mean no hashes, got %d", len(a.keyHashes))
	}
	code, reached := fireAuth(a, "", false)
	if code != http.StatusOK || !reached {
		t.Fatalf("auth-disabled: got code=%d reached=%v, want 200 and handler reached", code, reached)
	}
}

// TestAuthRejectsMissingAndWrongKey: once a key is set, a request with no token
// or a wrong token is 401 and never reaches the handler.
func TestAuthRejectsMissingAndWrongKey(t *testing.T) {
	a := NewAuth(nil, []string{"s3cret-key"})

	if code, reached := fireAuth(a, "", false); code != http.StatusUnauthorized || reached {
		t.Fatalf("no token: got code=%d reached=%v, want 401 and handler NOT reached", code, reached)
	}
	if code, reached := fireAuth(a, "wrong-key", true); code != http.StatusUnauthorized || reached {
		t.Fatalf("wrong token: got code=%d reached=%v, want 401 and handler NOT reached", code, reached)
	}
}

// TestAuthAcceptsValidKey: the correct token passes and reaches the handler.
func TestAuthAcceptsValidKey(t *testing.T) {
	a := NewAuth(nil, []string{"s3cret-key"})
	if code, reached := fireAuth(a, "s3cret-key", true); code != http.StatusOK || !reached {
		t.Fatalf("valid token: got code=%d reached=%v, want 200 and handler reached", code, reached)
	}
}

// TestAuthAcceptsAnyOfSeveralKeys: rotation needs an overlap window. Both the
// old and the new key must work at once, or every cutover is an outage.
func TestAuthAcceptsAnyOfSeveralKeys(t *testing.T) {
	a := NewAuth(nil, []string{"old-key", "new-key"})
	if code, _ := fireAuth(a, "old-key", true); code != http.StatusOK {
		t.Fatalf("old key during rotation: got %d, want 200", code)
	}
	if code, _ := fireAuth(a, "new-key", true); code != http.StatusOK {
		t.Fatalf("new key during rotation: got %d, want 200", code)
	}
	if code, _ := fireAuth(a, "retired-key", true); code != http.StatusUnauthorized {
		t.Fatalf("a key never issued: got %d, want 401", code)
	}
}

// TestAuthSchemeIsCaseInsensitive: real clients send "Bearer", "bearer", and
// "BEARER"; the scheme token is case-insensitive per RFC 7235, so all must work.
func TestAuthSchemeIsCaseInsensitive(t *testing.T) {
	a := NewAuth(nil, []string{"k"})
	for _, h := range []string{"Bearer k", "bearer k", "BEARER k"} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Authorization", h)
		rec := httptest.NewRecorder()
		a.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		a.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("scheme %q: got %d, want 200", h, rec.Code)
		}
	}
}

// TestAuthEmptyKeyEntriesDoNotEnableAuth: a config with only blank/whitespace
// entries must not silently turn auth on with an unmatchable key set, nor count
// as "keys present". Blank entries are dropped; the gate stays open (and warns).
func TestAuthEmptyKeyEntriesDoNotEnableAuth(t *testing.T) {
	a := NewAuth(nil, []string{"", "   ", "\t"})
	if len(a.keyHashes) != 0 {
		t.Fatalf("blank entries must not become keys, got %d hashes", len(a.keyHashes))
	}
	if code, reached := fireAuth(a, "", false); code != http.StatusOK || !reached {
		t.Fatalf("blank-only config must leave auth OFF: got code=%d reached=%v", code, reached)
	}
}
