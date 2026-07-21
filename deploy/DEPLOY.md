# Deploying an LXS node to a VPS (Hostinger or any x86-64 Linux)

This is the runbook for putting a **public testnet-grade node** on a server. It
is honest about what that means: a live node on a VPS is a real, correct step.
It is NOT "mainnet with real money" — that gate is external (security audit,
sustained public testnet, bug bounty) and is not something a deploy script
grants. Deploy this as a testnet node; promote to mainnet only after those gates.

## 0. What you are shipping

One static binary. `deploy/build.sh` cross-compiles it: `CGO_ENABLED=0
GOOS=linux GOARCH=amd64`, so the result is a single ELF with no shared-library
dependencies. Nothing on the server needs a Go toolchain.

```bash
./deploy/build.sh                 # -> deploy/lxs-linux-amd64 (+ its sha256)
```

## 1. Make the genesis ONCE (on your machine)

The chain's identity is (chainId, genesis hash). Every node must share the SAME
genesis file, or they form separate networks that "fail to peer" for a reason
that is not peering. Generate it once and distribute that one file.

```bash
./lxs init -out deploy/genesis.json -chain-id <YOUR_CHAIN_ID>
```

Save the founder keys it prints. They are stored nowhere else. The genesis file
is not secret; the keys are.

## 2. Copy the artifacts to the server

```bash
scp deploy/lxs-linux-amd64 deploy/genesis.json \
    deploy/lxs-node.service deploy/lxs.env.example \
    root@YOUR_VPS:/tmp/
```

## 3. Install (on the server)

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin lxs
sudo mkdir -p /opt/lxs /var/lib/lxs /etc/lxs
sudo mv /tmp/lxs-linux-amd64 /opt/lxs/lxs && sudo chmod +x /opt/lxs/lxs
sudo mv /tmp/genesis.json /etc/lxs/genesis.json
sudo mv /tmp/lxs.env.example /etc/lxs/lxs.env
sudo mv /tmp/lxs-node.service /etc/systemd/system/lxs-node.service
```

### Set the RPC API key (the one secret)

```bash
# generate a real key and put it in the env file, which stays chmod 600
KEY=$(openssl rand -hex 32)
sudo sed -i "s|CHANGE-ME-openssl-rand-hex-32|$KEY|" /etc/lxs/lxs.env
echo "SAVE THIS KEY: $KEY"
sudo chown -R lxs:lxs /var/lib/lxs /etc/lxs
sudo chmod 600 /etc/lxs/lxs.env
```

### Edit the node's flags

Open `/etc/systemd/system/lxs-node.service` and set, on `ExecStart`:
- `-p2p-seed` to a unique value per node,
- `-bootstrap` to the other nodes' multiaddrs (empty for the first node),
- add `-mine` only if this node should produce blocks,
- keep `-rpc 127.0.0.1:8545` and put a TLS reverse proxy in front for the public
  RPC — OR bind `0.0.0.0:8545` knowingly, which is safe ONLY because the API key
  in the env file is now required (unauthenticated = do not expose).

## 4. Firewall

Open the p2p port; keep the RPC port closed to the world unless it is behind a
proxy or you have deliberately chosen an authenticated public bind.

```bash
sudo ufw allow 30303/tcp        # p2p — peers must reach this
sudo ufw allow OpenSSH
# RPC stays closed; reach it via SSH tunnel or a TLS reverse proxy.
sudo ufw enable
```

## 5. Start and verify

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now lxs-node
journalctl -u lxs-node -f           # watch it come up

# from an SSH session on the box (RPC is localhost-bound):
curl -s http://127.0.0.1:8545 -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"chain_blockNumber","params":[]}'
```

The startup log MUST show `rpc auth ON — N key(s) required` and, if this node
mines, the producer role. If it shows `rpc auth OFF`, the env key did not load —
fix it before exposing anything.

## 6. Restart-safety (already proven locally)

`Restart=always` brings the process back after a crash; the node resumes from its
on-disk head (verified locally: kill + restart resumes at the persisted height,
not genesis). systemd is the process supervisor the node's in-process health
layer explicitly cannot be (it self-heals inside a living process, but a crashed
process cannot restart itself).

## 7. Upgrades

Build a new binary, copy it over the old one, `systemctl restart lxs-node`. The
data dir persists across the swap; the chain is not re-synced from scratch.

## Checklist before you call it done
- [ ] `journalctl` shows `rpc auth ON` and a climbing block number
- [ ] a second node peers and converges on the same head hash
- [ ] the RPC port is NOT world-reachable unauthenticated
- [ ] `/etc/lxs/lxs.env` is `chmod 600`, owned by `lxs`
- [ ] you saved the genesis founder keys and the API key somewhere safe
