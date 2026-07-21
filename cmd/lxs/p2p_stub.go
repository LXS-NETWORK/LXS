//go:build !libp2p

package main

import (
	"context"
	"fmt"

	"lxs/core"
	"lxs/health"
	"lxs/mempool"
	"lxs/types"
)

// startP2P without the libp2p build tag. Called only when -p2p-port is set,
// where it fails loudly rather than sit alone silently: a node told to peer that
// quietly runs isolated hides a missing build tag behind "started, no error".
// The error names the fix.
//
// p2pHandles here is only the type the shared signature needs; startP2P always
// errors before returning one.
type p2pHandles struct {
	Close      func() error
	Broadcast  func(*types.Transaction) error
	Peers      func() []health.PeerHealth
	Resync     func(context.Context) error
	Redial     func(context.Context) error
	Disconnect func(id string) error
}

func startP2P(_ context.Context, _ *core.Blockchain, _ *mempool.Mempool, _ *core.Producer, port int, _ string, _ []string) (*p2pHandles, error) {
	return nil, fmt.Errorf(
		"p2p: -p2p-port %d was set, but this binary has no networking: "+
			"rebuild with `go build -tags libp2p,pebble -o lxs ./cmd/lxs`", port)
}

func p2pID(_ []string) error {
	return fmt.Errorf("p2p-id needs networking: rebuild with `go build -tags libp2p,pebble -o lxs ./cmd/lxs`")
}
