package p2p

import (
	"math/rand"
	"sync"
)

// Switch is an in-process network fabric connecting InProc nodes. Deliberately
// worse than a real network: a fabric that delivers everything once, in order,
// only exercises a happy path real gossip never takes.
type Switch struct {
	mu    sync.RWMutex
	nodes map[PeerID]*InProc
	cfg   SwitchConfig
	rng   *rand.Rand
}

type SwitchConfig struct {
	// Duplicates is how many extra copies of each message to deliver. GossipSub
	// duplicates by design (a mesh delivers the same block from every meshed
	// peer), so code assuming once-only delivery corrupts state on a real network.
	Duplicates int

	// Shuffle delivers a node's messages in random order. Real gossip has no
	// cross-peer ordering: a child block sometimes arrives before its parent.
	Shuffle bool

	// DropRate drops this fraction of messages (0..1).
	DropRate float64

	Seed int64
}

func NewSwitch(cfg SwitchConfig) *Switch {
	return &Switch{
		nodes: make(map[PeerID]*InProc),
		cfg:   cfg,
		rng:   rand.New(rand.NewSource(cfg.Seed)),
	}
}

// Join creates a node attached to this switch.
func (s *Switch) Join(id PeerID) *InProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := &InProc{
		id:       id,
		sw:       s,
		subs:     make(map[Topic][]Handler),
		handlers: make(map[Protocol]RequestHandler),
	}
	s.nodes[id] = n
	return n
}

// get returns the node with id, or nil if it has left.
func (s *Switch) get(id PeerID) *InProc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[id]
}

// Leave disconnects a node, as a peer going offline would.
func (s *Switch) Leave(id PeerID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
}

// deliver fans a message out to every node except the sender. Synchronous by
// design: Publish returns only once every peer has processed the message, which
// keeps tests deterministic. Asynchrony is a transport property, and the
// transport is the substituted part; the logic under test must not depend on it.
func (s *Switch) deliver(from PeerID, topic Topic, data []byte) {
	s.mu.RLock()
	targets := make([]*InProc, 0, len(s.nodes))
	for id, n := range s.nodes {
		if id == from {
			continue
		}
		targets = append(targets, n)
	}
	dup := s.cfg.Duplicates
	shuffle := s.cfg.Shuffle
	drop := s.cfg.DropRate
	s.mu.RUnlock()

	if shuffle {
		s.mu.Lock()
		s.rng.Shuffle(len(targets), func(i, j int) {
			targets[i], targets[j] = targets[j], targets[i]
		})
		s.mu.Unlock()
	}

	for _, n := range targets {
		copies := 1 + dup
		for i := 0; i < copies; i++ {
			if drop > 0 {
				s.mu.Lock()
				skip := s.rng.Float64() < drop
				s.mu.Unlock()
				if skip {
					continue
				}
			}
			// Fresh copy per delivery: sharing one slice lets a handler mutate
			// what other peers receive, an aliasing bug a real socket cannot have.
			payload := append([]byte(nil), data...)
			n.dispatch(from, topic, payload)
		}
	}
}

// InProc is a Network backed by direct calls.
type InProc struct {
	id PeerID
	sw *Switch

	mu       sync.RWMutex
	subs     map[Topic][]Handler
	handlers map[Protocol]RequestHandler
	closed   bool

	// Errors returned by handlers, kept for tests to assert bad input was
	// rejected rather than silently swallowed.
	mu2         sync.Mutex
	handlerErrs []error
}

func (n *InProc) Self() PeerID { return n.id }

func (n *InProc) Publish(topic Topic, data []byte) error {
	n.mu.RLock()
	closed := n.closed
	n.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	n.sw.deliver(n.id, topic, data)
	return nil
}

func (n *InProc) Subscribe(topic Topic, h Handler) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return ErrClosed
	}
	n.subs[topic] = append(n.subs[topic], h)
	return nil
}

func (n *InProc) dispatch(from PeerID, topic Topic, data []byte) {
	n.mu.RLock()
	if n.closed {
		n.mu.RUnlock()
		return
	}
	hs := append([]Handler(nil), n.subs[topic]...)
	n.mu.RUnlock()

	for _, h := range hs {
		if err := safeHandle(h, from, data); err != nil {
			n.mu2.Lock()
			n.handlerErrs = append(n.handlerErrs, err)
			n.mu2.Unlock()
		}
	}
}

// HandlerErrors returns errors handlers returned. Test-only.
func (n *InProc) HandlerErrors() []error {
	n.mu2.Lock()
	defer n.mu2.Unlock()
	return append([]error(nil), n.handlerErrs...)
}

func (n *InProc) SetRequestHandler(proto Protocol, h RequestHandler) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return ErrClosed
	}
	n.handlers[proto] = h
	return nil
}

func (n *InProc) Request(to PeerID, proto Protocol, req []byte) ([]byte, error) {
	n.mu.RLock()
	closed := n.closed
	n.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}

	target := n.sw.get(to)
	if target == nil {
		return nil, ErrUnknownPeer
	}
	target.mu.RLock()
	h := target.handlers[proto]
	tclosed := target.closed
	target.mu.RUnlock()
	if tclosed {
		return nil, ErrClosed
	}
	if h == nil {
		return nil, ErrNoHandler
	}

	// Fresh copies across the boundary, as in deliver(): a real request is
	// serialised bytes, so neither side may retain a slice the other can mutate.
	resp, err := h(n.id, append([]byte(nil), req...))
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), resp...), nil
}

func (n *InProc) Peers() []PeerID {
	n.sw.mu.RLock()
	defer n.sw.mu.RUnlock()
	out := make([]PeerID, 0, len(n.sw.nodes))
	for id := range n.sw.nodes {
		if id != n.id {
			out = append(out, id)
		}
	}
	return out
}

func (n *InProc) Close() error {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	n.sw.Leave(n.id)
	return nil
}

var _ Network = (*InProc)(nil)
