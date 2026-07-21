//go:build pebble

package main

import "lxs/store"

func openDB(dir string) (store.KV, error) {
	return store.OpenPebble(dir)
}
