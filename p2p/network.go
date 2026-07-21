package p2p

import "errors"

var (
	ErrClosed       = errors.New("p2p: network closed")
	ErrUnknownTopic = errors.New("p2p: no subscriber for topic")
	ErrUnknownPeer  = errors.New("p2p: no such peer")
	ErrNoHandler    = errors.New("p2p: peer serves no such protocol")
)

// Protocol names a request/response conversation — the directed counterpart
// to a pubsub Topic. One peer, one answer. Maps directly onto a libp2p stream,
// so the adapter is a shim over host.NewStream / SetStreamHandler.
type Protocol string

// ProtoSync carries headers-first block sync.
const ProtoSync Protocol = "/lxs/sync/1.0.0"

// RequestHandler answers one inbound request; the returned bytes are the
// response. An error surfaces to the caller as a failed request and feeds peer
// scoring, so a handler must error on bad input rather than return a
// plausible-looking empty answer that hides it.
type RequestHandler func(from PeerID, req []byte) ([]byte, error)

// PeerID identifies a peer. Opaque by contract: a public-key multihash in the
// libp2p adapter, "node0" in the in-process network. Nothing above this package
// may parse it, or the transport can no longer be swapped.
type PeerID string

// Topic is a pubsub channel name.
type Topic string

const (
	TopicBlocks Topic = "/lxs/blocks/1.0.0"
	TopicTxs    Topic = "/lxs/txs/1.0.0"
)

// Network is the transport contract. Byte-level (publish bytes, receive bytes)
// to match libp2p pubsub, which keeps the protocol logic above it (dedup,
// orphan handling, validation) testable with no sockets, ports, or timing —
// same rationale as store.KV. Implementations must be safe for concurrent use.
type Network interface {
	// Self is this node's identity.
	Self() PeerID

	// Publish sends data to every peer subscribed to topic. Delivery is
	// best-effort and unordered: assume nothing about order, arrival, or count.
	Publish(topic Topic, data []byte) error

	// Subscribe registers a handler for a topic. The handler is called from a
	// network goroutine and must not block for long.
	Subscribe(topic Topic, h Handler) error

	// Peers currently connected.
	Peers() []PeerID

	// Request sends req to exactly one peer over proto and waits for its single
	// response. Unlike Publish, a request nobody answers is an error.
	Request(to PeerID, proto Protocol, req []byte) ([]byte, error)

	// SetRequestHandler registers the server side of a protocol. The handler
	// runs on a network goroutine and must be safe for concurrent use.
	SetRequestHandler(proto Protocol, h RequestHandler) error

	Close() error
}

// Handler receives one message. Returning an error marks the message invalid,
// which feeds peer scoring, so handlers must error on bad input rather than
// swallow it.
type Handler func(from PeerID, data []byte) error
