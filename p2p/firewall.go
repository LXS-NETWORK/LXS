package p2p

import (
	"fmt"
	"log"
)

// safeHandle runs a gossip Handler behind a recover firewall. Handlers process messages
// decoded from untrusted peers; a panic on hostile input (a malformed block/tx) in the
// per-topic goroutine would otherwise crash the whole node. A panic becomes an error,
// scored like any other handler failure, so a peer can grief at most itself.
func safeHandle(h Handler, from PeerID, data []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("p2p: recovered panic in gossip handler from %s: %v", from, r)
			err = fmt.Errorf("p2p: handler panic from %s: %v", from, r)
		}
	}()
	return h(from, data)
}

// safeRequest runs a RequestHandler behind the same firewall, so a malformed request can
// never crash the stream-handler goroutine — it fails the request instead.
func safeRequest(h RequestHandler, from PeerID, req []byte) (resp []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("p2p: recovered panic in request handler from %s: %v", from, r)
			resp, err = nil, fmt.Errorf("p2p: request handler panic from %s: %v", from, r)
		}
	}()
	return h(from, req)
}
