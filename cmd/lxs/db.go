//go:build !pebble

package main

import (
	"fmt"

	"lxs/store"
)

// openDB without the pebble build tag. Fails loudly rather than falling back to
// memory: a node told to persist that silently does not loses the chain on restart.
func openDB(dir string) (store.KV, error) {
	return nil, fmt.Errorf(
		"this binary was built without a disk backend.\n"+
			"  go get github.com/cockroachdb/pebble@latest\n"+
			"  go build -tags pebble ./cmd/lxs\n"+
			"then -datadir %s will work. Omit -datadir to run in memory.", dir)
}
