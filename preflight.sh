#!/usr/bin/env bash
# preflight.sh — what does p2p/ need that your tree may not have?
#
# Run this from the ROOT of your existing project, BEFORE copying any file.
# It reads; it never writes. The output is a checklist, not a diff.
#
#   ./preflight.sh
#
# The point is to turn "will this merge break my build?" from a question
# you answer by trying it into one you answer by reading.

set -uo pipefail

miss=0
ok()   { printf "  \033[32mOK\033[0m    %s\n" "$1"; }
bad()  { printf "  \033[31mMISS\033[0m  %s\n" "$1"; miss=$((miss+1)); }
info() { printf "        %s\n" "$1"; }

# find <pattern> <files...> — grep across the tree, ignoring _test files
has() {
  local pat="$1"; shift
  grep -rqE "$pat" --include='*.go' "$@" 2>/dev/null
}

echo
echo "=== module ==="
mod=$(grep -m1 '^module ' go.mod 2>/dev/null | awk '{print $2}')
if [ -z "$mod" ]; then
  bad "no go.mod found — are you in the project root?"
  exit 1
fi
if [ "$mod" = "lxs" ]; then
  ok "module is 'lxs' — the p2p files' imports will resolve as-is"
else
  bad "module is '$mod', but the p2p files import \"lxs/core\", \"lxs/types\", \"lxs/common\""
  info "fix (one command, reversible, do it on a branch):"
  info "  go mod edit -module lxs"
  info "  grep -rl '\"$mod/' --include='*.go' . | xargs sed -i '' 's|\"$mod/|\"lxs/|g'   # macOS sed"
fi

echo
echo "=== what p2p/gossip.go needs from core ==="
has 'ErrUnknownParent' core/     && ok "core.ErrUnknownParent"        || bad "core.ErrUnknownParent  (phase 3a)"
has 'ErrKnownBlock'    core/     && ok "core.ErrKnownBlock"           || bad "core.ErrKnownBlock     (phase 3a)"
has 'func \(bc \*Blockchain\) HasBlock' core/ && ok "Blockchain.HasBlock" || bad "Blockchain.HasBlock  (phase 3a)"
has 'func \(bc \*Blockchain\) InsertBlock' core/ && ok "Blockchain.InsertBlock" || bad "Blockchain.InsertBlock"

echo
echo "=== what p2p tests need ==="
has 'func NewBlockchain\(db store\.KV' core/ && ok "NewBlockchain(store.KV, *Genesis, Options)  (phase 3.5)" \
  || bad "NewBlockchain(store.KV, ...)  — your tree predates persistence (phase 3.5)"
has 'func \(bc \*Blockchain\) StateAt' core/ && ok "Blockchain.StateAt" || bad "Blockchain.StateAt (phase 3a)"
has 'TotalSupply' core/genesis.go && ok "Genesis.TotalSupply" || bad "Genesis.TotalSupply  (branding drop)"
has 'func LXS\(' common/ && ok "common.LXS()" || bad "common.LXS()  (branding drop)"

echo
echo "=== the ONE change p2p forces on core ==="
if has 'func \(p \*Producer\) SetOnBlock' core/; then
  ok "Producer.SetOnBlock — already present"
else
  bad "Producer.SetOnBlock — must be added (3 lines, step 5)"
  info "this is the only line of core that p2p touches, and it is a HOOK:"
  info "core still imports nothing from p2p. Same shape as BindMempool."
fi

echo
echo "=== things that CHANGE consensus if you take them ==="
if has 'checkLowS|ErrHighS' crypto/; then
  ok "crypto: low-s enforced (malleability fix present)"
else
  info "crypto: low-s NOT enforced — signatures are malleable."
  info "  Taking the fix is correct but it REJECTS signatures your"
  info "  current tree accepts. Devnet: harmless. Note it and move on."
fi
if has 'TxTypeTransfer' types/; then
  ok "types: typed envelope present"
else
  info "types: no typed envelope. Taking it changes SigningHash, so EVERY"
  info "  previously signed tx becomes invalid. Genesis hash is unaffected"
  info "  (genesis has no txs), so your existing datadir still opens."
fi

echo
if [ "$miss" -eq 0 ]; then
  printf "\033[32mReady.\033[0m p2p/ will compile against this tree.\n"
else
  printf "\033[31m%d prerequisite(s) missing.\033[0m Fix those before copying p2p/.\n" "$miss"
fi
echo
exit 0
