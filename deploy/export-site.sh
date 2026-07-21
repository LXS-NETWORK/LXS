#!/usr/bin/env bash
# export-site.sh — produce a ready-to-host copy of the launchpad site.
#
# There is ONE source of truth: web/launchpad (the same files embedded in the binary, so a
# node can serve them too). You never edit two copies — edit web/launchpad only. When you
# want to put the public site online, run this: it copies web/launchpad with YOUR node's RPC
# and factory baked in, so it works standalone. Re-run it whenever you deploy; the output is
# always freshly derived from the source, so it can never drift.
#
# Upload the output folder anywhere static: a normal host, or IPFS/ENS for a survivable,
# censorship-resistant site (e.g. `npx pinme upload <out>` pins it to IPFS in one command).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/web/launchpad"
RPC=""; FACTORY=""; CHAINID="2254"; OUT="$ROOT/site-dist"

while [ $# -gt 0 ]; do
  case "$1" in
    -rpc) RPC="$2"; shift 2;;
    -factory) FACTORY="$2"; shift 2;;
    -chainid) CHAINID="$2"; shift 2;;
    -out) OUT="$2"; shift 2;;
    *) echo "unknown arg: $1"; exit 1;;
  esac
done

if [ -z "$RPC" ] || [ -z "$FACTORY" ]; then
  echo "usage: deploy/export-site.sh -rpc <PUBLIC_RPC_URL> -factory <PUMPFACTORY_ADDR> [-chainid N] [-out DIR]"
  echo "  e.g. deploy/export-site.sh -rpc https://rpc.mylxs.net -factory 0xabc... "
  exit 1
fi

rm -rf "$OUT"; mkdir -p "$OUT"
cp "$SRC"/* "$OUT"/

CFG="<script>window.LXS_CONFIG={\"RPC_URL\":\"$RPC\",\"CHAIN_ID\":$CHAINID,\"FACTORY_ADDRESS\":\"$FACTORY\"};</script>"
python3 - "$OUT/index.html" "$CFG" <<'PY'
import sys
path, cfg = sys.argv[1], sys.argv[2]
html = open(path).read()
if "</head>" not in html:
    sys.exit("index.html has no </head> to inject config into")
open(path, "w").write(html.replace("</head>", cfg + "</head>", 1))
PY

echo "ready: $OUT"
echo "  RPC_URL=$RPC  CHAIN_ID=$CHAINID  FACTORY=$FACTORY"
echo "upload the CONTENTS of that folder to your host, or pin to IPFS:  npx pinme upload \"$OUT\""
