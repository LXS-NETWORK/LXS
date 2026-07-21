# Disk backend (Pebble)

The default build has no storage dependency, so the test suite runs against the
in-memory KV in milliseconds. Disk persistence is behind the `pebble` build tag.

```bash
go get github.com/cockroachdb/pebble@latest
go build -tags pebble -o lxs ./cmd/lxs
go test  -tags pebble ./store/      # includes the interface conformance check

./lxs init
./lxs node -datadir ./data -block-time 1s
# stop and restart: same head, same balances, same tx index
```

`store/pebble.go` is a ~150-line adapter behind the `store.KV` interface; nothing
above that interface knows Pebble exists. The conformance assertions in
`store/pebble_conformance_test.go` fail at compile time if a method signature drifts.

## Two non-cosmetic settings

**`Sync: true` by default.** Pebble's `NoSync` is faster and survives a process
crash but not a power cut — the atomic batch guarantee is void if the OS still
holds the write in a buffer when the machine dies. Disable only on a devnet.

**`prefixUpperBound`.** Pebble iterators need an explicit upper bound; a nil bound
scans to the end of the keyspace. Without it, `Iterate("a")` also returns the
`b`/`c`/`d`/`r`/`t` tables — no error, just rows from the wrong table, surfacing
later as a wrong state root.
