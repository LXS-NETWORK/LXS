#!/usr/bin/env bash
# graduate-e2e.sh — LIVE end-to-end proof of the graduation pipeline against a REAL
# running node and the REAL graduate daemon binary (no mocks in the plumbing).
#
# It funds an operator, starts a mining node, deploys the graduation contracts + a coin +
# a stand-in router, calls graduate() on-chain, then runs the actual `graduate` daemon and
# waits for it to build the pool. This exercises the daemon's entire LIVE path — eth_getLogs,
# deploy + receipt, sequential signed submits, the reconciliation loop across real blocks,
# and state persistence. The only thing it does NOT cover is Base's specific gas model
# (EIP-1559 base fee + L1 data fee); that needs a real Base testnet.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
WORK="$(mktemp -d)"
NODE_PID=""; DAEMON_PID=""
cleanup(){ [ -n "$NODE_PID" ] && kill "$NODE_PID" 2>/dev/null || true
           [ -n "$DAEMON_PID" ] && kill "$DAEMON_PID" 2>/dev/null || true
           rm -rf "$WORK"; }
trap cleanup EXIT

RPC="http://127.0.0.1:8545"; CHAINID=1337
TOKENAMT="1000000000000000000000"   # 1000 coin committed to the pool
LXSAMT="5000000000000000000"        # 5 LXS committed
MINLIQ="1000000000000000000"        # 1 LXS gate
SUPPLY="1000000000000000000000000"  # 1,000,000 coin minted to the creator

echo "==> build binaries"
go build -o "$WORK/lxs"      ./cmd/lxs
go build -o "$WORK/graduate" ./cmd/graduate
go build -o "$WORK/gradhex"  ./cmd/gradhex
LXS="$WORK/lxs"; HEX="$WORK/gradhex"

echo "==> operator key + funded genesis"
KG="$("$LXS" keygen)"
OPKEY="$(echo "$KG" | awk '/private key/{print $3}')"
OPADDR="$(echo "$KG" | awk '/address/{print $2}')"
echo "    operator $OPADDR"
"$LXS" init -out "$WORK/genesis.json" -founder-addr "$OPADDR" -chain-id "$CHAINID" -difficulty 4000 >/dev/null

echo "==> start mining node"
"$LXS" node -genesis "$WORK/genesis.json" -rpc 127.0.0.1:8545 -mine >"$WORK/node.log" 2>&1 &
NODE_PID=$!
for i in $(seq 1 60); do
  curl -s -m1 -X POST "$RPC" -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' 2>/dev/null | grep -q '"result"' && break
  sleep 0.5
done

send_deploy(){ # $1=initcode $2=gas  -> prints contract address
  "$LXS" send -key "$OPKEY" -data "$1" -gas "$2" -wait 2>/dev/null | awk '/^contract/{print $2}'
}
send_call(){ # $1=to $2=data $3=value $4=gas
  "$LXS" send -key "$OPKEY" -to "$1" -data "$2" -value "$3" -gas "$4" -wait >/dev/null 2>&1
}

echo "==> deploy wLXS, vault, router, coin"
WLXS="$(send_deploy "$("$HEX" wlxs-init "$OPADDR")" 2000000)"
VAULT="$(send_deploy "$("$HEX" vault-init "$OPADDR" "$MINLIQ")" 2000000)"
ROUTER="$(send_deploy "$("$HEX" mockrouter-init)" 1000000)"
COIN="$(send_deploy "$("$HEX" usertoken HITMAN HIT "$SUPPLY")" 2000000)"
echo "    wLXS=$WLXS vault=$VAULT router=$ROUTER coin=$COIN"
[ -n "$WLXS$VAULT$ROUTER$COIN" ] || { echo "FAIL: a deploy returned no address"; exit 1; }

echo "==> approve vault + graduate() on-chain"
send_call "$COIN"  "$("$HEX" approve "$VAULT" "$TOKENAMT")" 0        100000
send_call "$VAULT" "$("$HEX" graduate "$COIN" "$TOKENAMT")" "$LXSAMT" 400000

echo "==> run the REAL graduate daemon"
"$WORK/graduate" -operator-key "$OPKEY" \
  -lxs-rpc "$RPC" -lxs-chainid "$CHAINID" -vault "$VAULT" \
  -base-rpc "$RPC" -base-chainid "$CHAINID" -wlxs "$WLXS" -router "$ROUTER" \
  -confirmations 0 -interval 1s -store "$WORK/state.json" >"$WORK/daemon.log" 2>&1 &
DAEMON_PID=$!

echo "==> wait for POOL SEEDED (up to 90s)"
ok=0
for i in $(seq 1 90); do
  if grep -q "POOL SEEDED" "$WORK/daemon.log"; then ok=1; break; fi
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then echo "daemon exited early"; break; fi
  sleep 1
done

echo "----- daemon log -----"; cat "$WORK/daemon.log"; echo "----------------------"
[ "$ok" = 1 ] || { echo "FAIL: pool was not seeded in time"; exit 1; }

# Independent on-chain check: the router (pool) must hold both wrapped legs.
WMEME="$(awk '/wMeme for/{print $NF}' "$WORK/daemon.log" | tail -1)"
[ -n "$WMEME" ] || { echo "FAIL: could not find the deployed wMeme address in the log"; exit 1; }
POOL_MEME="$("$LXS" call -to "$WMEME" -data "$("$HEX" balanceof "$ROUTER")" | tr -d '\n')"
POOL_WLXS="$("$LXS" call -to "$WLXS"  -data "$("$HEX" balanceof "$ROUTER")" | tr -d '\n')"
MEME_DEC="$(python3 -c "print(int('$POOL_MEME',16))")"; WLXS_DEC="$(python3 -c "print(int('$POOL_WLXS',16))")"
echo "==> pool holdings: wMeme=$MEME_DEC wLXS=$WLXS_DEC (want $TOKENAMT / $LXSAMT)"
[ "$MEME_DEC" = "$TOKENAMT" ] && [ "$WLXS_DEC" = "$LXSAMT" ] || { echo "FAIL: pool legs mismatch"; exit 1; }

echo "==> PASS: live graduation seeded the wMeme/wLXS pool end-to-end"
