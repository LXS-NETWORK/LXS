# Graduating a coin to a Base pool (→ external DEXs / Coinbase)

This is the operator runbook for **graduation**: taking a launchpad coin from its
internal LXS bonding curve to a real Uniswap pool on **Base**, so it can be indexed
by Coinbase's DEX trading (0x/1inch) and bought with real money. Every coin stays
**LXS-denominated** — the Base pool is `wMeme / wLXS`, so buying a graduated coin
still creates demand for LXS.

Honest scope, up front:

- This is **off-chain operator infrastructure**, NOT part of the immutable chain. The
  two contracts here are deployed by you, and the daemon is a process you run and can
  change/tune anytime. Nothing here is baked into the coin binary miners run.
- It moves value and holds a **hot key** on Base. The contracts bound a compromised
  operator (locked backing, operator-only release, nonce-once), but the key is a live
  secret — protect it.
- The whole pipeline is proven end-to-end locally (`scripts/graduate-e2e.sh`). The one
  thing local cannot exercise is **Base's gas market** (EIP-1559 base fee + L1 data
  fee); `-base-gas-mult` gives headroom for it. Do a Base **Sepolia** dry-run before
  pointing it at mainnet money — prudence, not a correctness gate.

## What you are shipping

- **GraduationVault** — on LXS. Locks a creator's committed LXS (≥ a min-liquidity gate)
  and a chunk of their coin, and emits `Graduated`. Deploy **once**.
- **WrappedLXS (wLXS)** — on Base. The shared bridged-LXS ERC-20. This is the SAME wLXS
  the peg / go-to-market uses — deploy it once and reuse it for every coin.
- **WrappedToken (wMeme)** — on Base, one per graduated coin. You do NOT deploy these by
  hand: the `graduate` daemon deploys each one automatically (with the coin's own
  name/symbol) when it sees a `Graduated` event.
- **UniswapV2Router02** — already live on Base at
  `0x4752ba5dbc23f44d87826276bf6fd6b1c372ad24`. You do NOT deploy a DEX; the daemon calls
  this canonical router. (On Base Sepolia use that network's router address instead.)
- **graduate** — the daemon that watches `Graduated` and builds the pool.

The helper `cmd/gradhex` prints the deploy bytecode / calldata for the steps below, reusing
the same `contracts` code the node uses:

```bash
go build -o gradhex ./cmd/gradhex
```

## 1. Prerequisites

- A running LXS node with RPC (your public node — see `deploy/DEPLOY.md`).
- The launchpad (`PumpFactory`) already deployed and coins trading on it.
- A **Base RPC** URL and an **operator key funded with Base ETH** for gas.
- The shared **wLXS** deployed on Base (from the peg / go-to-market). If it does not exist
  yet, deploy it once:

```bash
# on Base (operator key = the one the daemon will use)
lxs send -rpc <BASE_RPC> -key <OPERATOR_KEY> \
  -data "$(./gradhex wlxs-init <OPERATOR_ADDR>)" -gas 2000000 -wait
# note the printed `contract <addr>` — this is wLXS
```

## 2. Deploy the GraduationVault on LXS (once)

`minLiquidity` is your on-chain "at least ~1 pound of LXS" gate, in **wei**. It is the
floor a creator must commit to graduate — pick the LXS amount you want to require. Example
below uses 1 LXS = `1000000000000000000`.

```bash
lxs send -rpc <LXS_RPC> -key <OPERATOR_KEY> \
  -data "$(./gradhex vault-init <OPERATOR_ADDR> 1000000000000000000)" \
  -gas 2000000 -wait
# note the printed `contract <addr>` — this is the GraduationVault
```

Read it back to confirm:

```bash
lxs call -rpc <LXS_RPC> -to <VAULT> -data 0x252cf2d2   # minLiquidity()
```

## 3. Wire the "Graduate" action (the creator's two calls)

A coin graduates when its creator commits liquidity. This is two txs the creator signs —
wire them behind a **"Graduate" button** on the website. Say the creator commits
`TOKENAMT` of their coin (wei) and `LXSAMT` of LXS (wei, ≥ minLiquidity):

```bash
# 1) let the vault pull the committed coin
lxs send -rpc <LXS_RPC> -key <CREATOR_KEY> -to <COIN> \
  -data "$(./gradhex approve <VAULT> <TOKENAMT>)" -gas 100000 -wait

# 2) graduate: locks the coin + LXS, emits Graduated
lxs send -rpc <LXS_RPC> -key <CREATOR_KEY> -to <VAULT> \
  -data "$(./gradhex graduate <COIN> <TOKENAMT>)" -value <LXSAMT> -gas 400000 -wait
```

If `LXSAMT` is below `minLiquidity`, step 2 reverts — that is the gate working.

## 4. Run the graduate daemon

The daemon does the Base side automatically for every `Graduated`: deploy wMeme, mint wMeme
and wLXS against the locked backing, approve the router, and `addLiquidity`. Run it as a
long-lived service (systemd, like the node) with the operator key.

```bash
graduate \
  -operator-key <OPERATOR_KEY> \
  -lxs-rpc  <LXS_RPC>  -lxs-chainid  <LXS_CHAIN_ID>  -vault <VAULT> \
  -base-rpc <BASE_RPC> -base-chainid 8453           -wlxs  <WLXS> \
  -router 0x4752ba5dbc23f44d87826276bf6fd6b1c372ad24 \
  -confirmations 12 -base-gas-mult 2 -interval 15s \
  -store /var/lib/lxs/graduate-state.json
```

Flag notes:

- `-confirmations` — blocks to wait before acting, for reorg safety. Use **12** on a real
  chain. Use **0** for a local single-node test, because the LXS producer skips empty
  blocks, so `head` never moves past the event block on its own.
- `-base-gas-mult` — multiplies the suggested Base gas price for headroom on the L2
  base-fee market (default 2). Overpayment is refunded; only the base fee is burned.
- `-store` — the daemon persists its cursor + per-coin wMeme addresses + seeded set here,
  so a restart resumes mid-graduation. Put it on durable storage.

The daemon is a **reconciliation loop**: one on-chain action per tick, each gated by
on-chain state, so a restart or a re-run never repeats a completed step. The mints are
nonce-guarded on-chain; the one non-idempotent step (`addLiquidity`) is marked done only
after its receipt confirms.

## 5. Verify

Local dry-run of the whole pipeline against a real node + the real daemon:

```bash
bash scripts/graduate-e2e.sh
# ... => PASS: live graduation seeded the wMeme/wLXS pool end-to-end
```

In production, watch the daemon log per graduation:

```
grad N: deploying wMeme "<NAME>"/"<SYM>" tx ...
grad N: minted wMeme ... / minted wLXS ...
grad N: approved wMeme->router / approved wLXS->router
grad N: POOL SEEDED (wMeme/wLXS) — coin <COIN> is now on a Base DEX
```

After "POOL SEEDED", the `wMeme/wLXS` pair exists on Base and 0x/1inch will index it,
typically within an hour, into Coinbase's DEX trading.

## 6. Before mainnet money — the honest gates

- **Base Sepolia dry-run.** Run steps 2–5 against Base Sepolia (its router address, testnet
  wLXS, a Base Sepolia RPC + faucet key). This is the only way to exercise Base's real gas
  market. Not a correctness gate — the logic is proven — but do it before real funds.
- **feeRecipient = your address.** Unrelated to graduation but a deploy-time revenue
  reminder: when you deploy the launchpad `PumpFactory`, set `feeRecipient` to YOUR address,
  not the burn address, or you give away the 1% trading fee forever.
- **Pool ratio = opening price.** The `TOKENAMT : LXSAMT` a creator commits sets the pool's
  opening price. The daemon seeds with `amountMin = 0` because on a fresh pair there is
  nothing to slip against; if a pair already exists, addLiquidity respects its ratio.

## Selector reference

| Contract | Function | Selector / topic |
|---|---|---|
| GraduationVault | `graduate(address,uint256)` | `0xec8dc754` |
| GraduationVault | `minLiquidity()` | `0x252cf2d2` |
| GraduationVault | `lxsReserve()` | `0x4d34b0b7` |
| GraduationVault | `tokenReserve(address)` | `0x54d39008` |
| GraduationVault | `releaseLxs(uint256,address,uint256)` | `0x45dfcdbb` |
| GraduationVault | `releaseToken(uint256,address,address,uint256)` | `0x055c485a` |
| GraduationVault | `Graduated(...)` topic0 | `0x55e5ad396a368af4f393e3b6e139bc89f5dee553f4f9482ab62281a0bcf0e7ad` |
| WrappedToken | `mint(uint256,address,uint256)` | `0x836a1040` |
| WrappedToken | `redeem(uint256)` | `0xdb006a75` |
| UniswapV2Router02 | `addLiquidity(...)` | `0xe8e33700` |
