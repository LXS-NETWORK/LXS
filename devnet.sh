#!/usr/bin/env bash
# devnet.sh — bring up a 4-node LXS devnet on one laptop.
#
# Phase 3b. This script predates the libp2p code on purpose: if you cannot
# run four nodes on one machine and reason about them, you cannot debug a
# network between them. The network is now live (3b.2) — nodes discover over
# mDNS, gossip blocks, and sync headers-first — so the four converge.
#
# Usage:
#   ./devnet.sh init     # one shared genesis for all four nodes
#   ./devnet.sh up       # start node0 (producer) + node1..3 (followers)
#   ./devnet.sh status   # ask every node where it thinks it is
#   ./devnet.sh down
#   ./devnet.sh clean    # delete all state

set -euo pipefail

BIN=${BIN:-./lxs}
DIR=${DIR:-./devnet}
N=${N:-4}
MINERS=${MINERS:-1}   # how many of the N nodes mine; PoW + HeaviestChain make >1 safe
BLOCK_TIME=${BLOCK_TIME:-2s}

# Port layout. Wide gaps so a stray port never collides with an
# unrelated service and leaves you debugging the wrong thing.
rpc_port() { echo $((18545 + $1)); }
p2p_port() { echo $((19000 + $1)); }

init() {
  mkdir -p "$DIR"
  # ONE genesis, shared by every node.
  #
  # Not four genesis files. The chain's identity is (chainId, genesis
  # hash), and the database refuses to open on a mismatch — which means
  # four separately-generated genesis files produce four networks that
  # cannot talk, and the error will look like a peering failure. It is
  # not. It is you.
  "$BIN" init -out "$DIR/genesis.json" > "$DIR/genesis-keys.txt"
  cat "$DIR/genesis-keys.txt"
  echo
  echo "Genesis + funded keys written to $DIR/. The keys are in genesis-keys.txt"
  echo "and nowhere else."
}

up() {
  [ -f "$DIR/genesis.json" ] || { echo "run '$0 init' first"; exit 1; }

  # Deterministic p2p identities, so every node's dial address is known before
  # any node starts. This is what makes explicit bootstrap peering possible —
  # and it is why the four converge reliably instead of leaving mDNS to chance.
  declare -a PID
  for i in $(seq 0 $((N - 1))); do
    PID[$i]=$("$BIN" p2p-id -seed "lxs-node-$i")
  done

  for i in $(seq 0 $((N - 1))); do
    # Every node gets its OWN datadir. Sharing one would not merely
    # conflict — Pebble takes an exclusive lock, so the second node fails
    # to start, and the error will not say "you shared a datadir".
    d="$DIR/node$i"
    mkdir -p "$d"

    # The first MINERS nodes mine; the rest follow.
    #
    # Before phase 4 this was locked to ONE miner: HighestBlock made height
    # free, so competing miners just thrashed the fork choice. PoW changed
    # that — work is not free, and HeaviestChain resolves a race by total
    # difficulty. So MINERS>1 is now a real thing to watch: miners compete,
    # occasionally fork, and the network converges on the heaviest chain.
    if [ "$i" -lt "$MINERS" ]; then
      mine="-mine -empty-blocks"
      role="MINER"
    else
      mine=""
      role="follower"
    fi

    # Bootstrap list: every OTHER node's multiaddr. Each node dials all of
    # them and keeps them connected, so the mesh does not depend on discovery.
    boot=""
    for j in $(seq 0 $((N - 1))); do
      [ "$j" -eq "$i" ] && continue
      boot="${boot:+$boot,}/ip4/127.0.0.1/tcp/$(p2p_port $j)/p2p/${PID[$j]}"
    done

    "$BIN" node \
      -genesis "$DIR/genesis.json" \
      -datadir "$d/db" \
      -rpc "127.0.0.1:$(rpc_port $i)" \
      -p2p-port "$(p2p_port $i)" \
      -p2p-seed "lxs-node-$i" \
      -bootstrap "$boot" \
      -no-default-bootstrap \
      -block-time "$BLOCK_TIME" \
      $mine \
      > "$d/node.log" 2>&1 &

    echo $! > "$d/pid"
    printf "node%d  %-8s  rpc :%s  p2p :%s  pid %s\n" \
      "$i" "$role" "$(rpc_port $i)" "$(p2p_port $i)" "$(cat "$d/pid")"
  done

  echo
  echo "Logs: $DIR/node*/node.log"
  echo "All four should climb together on one head hash: node0 produces,"
  echo "node1..3 follow via block gossip and headers-first sync (phase 3b.2)."
}

status() {
  for i in $(seq 0 $((N - 1))); do
    port=$(rpc_port $i)
    resp=$(curl -s --max-time 2 "http://127.0.0.1:$port" \
      -H 'content-type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"chain_blockNumber","params":[]}' 2>/dev/null || true)
    if [ -z "$resp" ]; then
      printf "node%d  DOWN\n" "$i"
      continue
    fi
    height=$(echo "$resp" | sed -n 's/.*"result":"\([^"]*\)".*/\1/p')
    hresp=$(curl -s --max-time 2 "http://127.0.0.1:$port" \
      -H 'content-type: application/json' \
      -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"chain_getBlockByNumber\",\"params\":[\"$height\",false]}" 2>/dev/null || true)
    hash=$(echo "$hresp" | sed -n 's/.*"hash":"\(0x[0-9a-f]\{10\}\)[0-9a-f]*".*/\1/p')
    printf "node%d  height %-6s head %s...\n" "$i" "$height" "${hash:-?}"
  done
  echo
  echo "The test that matters: same height => same head hash. Different"
  echo "hashes at the same height means the network partitioned."
}

down() {
  for i in $(seq 0 $((N - 1))); do
    f="$DIR/node$i/pid"
    [ -f "$f" ] || continue
    kill "$(cat "$f")" 2>/dev/null || true
    rm -f "$f"
  done
  echo "stopped"
}

clean() { down; rm -rf "$DIR"; echo "cleaned"; }

case "${1:-}" in
  init)   init ;;
  up)     up ;;
  status) status ;;
  down)   down ;;
  clean)  clean ;;
  *) echo "usage: $0 {init|up|status|down|clean}"; exit 1 ;;
esac
