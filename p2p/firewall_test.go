package p2p

import (
	"errors"
	"testing"
)

// A gossip/request handler processes untrusted peer input; a panic in it (a decode bug on
// a hostile message) must be contained, not crash the node's goroutine. The firewall turns
// a panic into a scored handler error.
func TestFirewallRecoversHandlerPanic(t *testing.T) {
	boom := func(PeerID, []byte) error { var p *int; _ = *p; return nil } // nil deref -> panic
	if err := safeHandle(boom, "peer", []byte("x")); err == nil {
		t.Fatal("safeHandle let a handler panic escape instead of returning an error")
	}

	boomReq := func(PeerID, []byte) ([]byte, error) { panic("kaboom") }
	resp, err := safeRequest(boomReq, "peer", []byte("x"))
	if err == nil || resp != nil {
		t.Fatal("safeRequest let a request-handler panic escape")
	}

	// a normal handler is unaffected.
	ok := func(PeerID, []byte) error { return nil }
	if err := safeHandle(ok, "peer", nil); err != nil {
		t.Fatalf("safeHandle wrapped a clean handler into an error: %v", err)
	}
	fail := func(PeerID, []byte) error { return errors.New("normal error") }
	if err := safeHandle(fail, "peer", nil); err == nil {
		t.Fatal("safeHandle swallowed a normal handler error")
	}
}
