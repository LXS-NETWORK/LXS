# LXS

A proof-of-work EVM Layer-1, written from scratch in Go.

LXS pairs an Ethereum-compatible execution layer (real `solc`/Istanbul bytecode)
with a Bitcoin-style monetary policy (mined supply, halving, fee burn). MetaMask
connects to it directly; any Solidity contract that targets Ethereum runs on it.

## Features

- **PoW consensus** with per-block difficulty retargeting and heaviest-chain fork
  choice (accumulated difficulty). Target block time: 4 minutes.
- **EVM interpreter** running real `solc` (Istanbul) bytecode: arithmetic incl.
  SHA3, Istanbul-gas storage, CREATE/CREATE2, CALL/DELEGATECALL/STATICCALL,
  precompiles 0x01–0x04, LOG* in the receipt root, RETURNDATA, EXTCODECOPY.
- **Deflation**: a fixed share of every transaction fee is burned at the consensus
  level and folded into the state root.
- **P2P** over libp2p: gossip block/tx propagation, headers-first sync, fork-aware
  catch-up, peer scoring, per-peer rate limiting.
- **JSON-RPC**: an Ethereum `eth_*` facade plus `eth_getLogs`, hardened with
  optional API-key auth, a CORS allowlist, and per-IP rate limiting.
- **Two token primitives**: `create-token` (a fixed-supply ERC-20 in one command)
  and a bonding-curve launchpad (`PumpFactory`) for instantly tradeable coins.
- **Self-monitoring** node layer: health checks, self-healing, and adaptive tuning
  within the running process.

## Build

```bash
# Fast build — no storage/networking deps; the whole protocol tests in milliseconds
go build ./... && go test ./...

# Full node build (disk persistence + libp2p networking)
go build -tags "pebble,libp2p" -o lxs ./cmd/lxs
```

## Run

```bash
./lxs init -founder-addr 0xYourAddress   # write genesis
./lxs node -datadir ./data -mine -coinbase 0xYourAddress   # a mining seed node

# in another terminal
./lxs balance -addr 0x...
./lxs send -key 0x... -to 0x... -value 1000000000000000000 -wait
./lxs create-token -key 0x... -name "My Coin" -symbol MYC -supply 1000000
./lxs receipt -tx 0x...
```

A local 4-node network: `./devnet.sh init && ./devnet.sh up && ./devnet.sh status`.

Subcommands: `keygen · init · node · p2p-id · balance · send · call ·
create-token · block · receipt · demo · reorg-demo`.

## Tokenomics

| Quantity | Value |
|---|---|
| Genesis pre-mine | 20,000,000 LXS |
| Block reward (era 0) | 50 LXS + all tx fees, 100% to the miner |
| Block time | 4 minutes (difficulty-targeted) |
| Halving | every 1,000,000 blocks |
| Total mined, all eras | ≈ 100,000,000 LXS |
| Effective max supply | ≈ 120,000,000 LXS |
| Fee burn | 20% of every transaction fee |

Supply is bounded by halving (a converging geometric series) and can shrink once
the fee burn outpaces issuance.

## Architecture

Dependencies point one way:
`store, common <- crypto <- types <- state <- core <- rpc <- node`.
`core` never imports `mempool` or `p2p`; cross-layer signals are hooks.

```
store/     KV interface, in-memory impl, Pebble adapter (build-tagged)
common/    Hash, Address, keccak256, canonical binary encoder
crypto/    secp256k1 keys, address derivation, recoverable signatures
types/     Transaction, Block, Header, Receipt, Merkle tree
state/     account state, state root, the state transition function
vm/        EVM interpreter
mempool/   pending txs, nonce ordering, gas-price floor
core/      genesis, block tree, fork choice, reorg, PoW, persistence
p2p/       libp2p gossip, headers-first sync, peer scoring, rate limiting
rpc/       JSON-RPC server (eth_* facade, eth_getLogs), auth/CORS/rate-limit
node/      block production loop, HTTP lifecycle, faucet
health/    self-monitoring / self-healing / adaptive tuning
contracts/ shipped Solidity (ERC-20, bonding curve, LXS<->Base peg) + embedders
cmd/lxs/   CLI: node and wallet
```

Storage and wire formats use JSON; consensus hashing uses a canonical binary
encoder. Every load re-derives the hash and refuses on mismatch.

## Testing

```bash
go test ./...                                   # protocol, in-memory, fast
go test -race ./...                             # data-race detector
go test -tags pebble ./store/                   # disk backend
go build -tags "pebble,libp2p" ./...            # full build
```

## Status

The protocol is feature-complete and tested: keys/tx/state/blocks/mempool, fork
choice/reorg/persistence, p2p gossip and sync, PoW and halving, the EVM, and
Ethereum RPC compatibility. Build tags keep the default build dependency-free so
the full protocol test suite runs in milliseconds.

## License

MIT. See [LICENSE](LICENSE).
