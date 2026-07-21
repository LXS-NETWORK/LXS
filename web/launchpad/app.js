// ============================ CONFIG ============================
// Defaults for a standalone-hosted copy; when this page is served BY a node (lxs node
// -launchpad) the node injects window.LXS_CONFIG (RPC_URL/CHAIN_ID/FACTORY_ADDRESS) so the
// same file works unedited on every node — the UI is as decentralized as the chain.
const CONFIG = Object.assign({
  RPC_URL: "http://127.0.0.1:8545",   // your PUBLIC LXS RPC (the server node)
  CHAIN_ID: 2254,
  FACTORY_ADDRESS: "",                // the deployed PumpFactory address (fill after you deploy it)
  FAUCET_URL: "",                     // gas faucet; empty = RPC_URL + "/faucet" (node -faucet)
}, (typeof window !== "undefined" && window.LXS_CONFIG) || {});
const FAUCET_URL = CONFIG.FAUCET_URL || (CONFIG.RPC_URL.replace(/\/+$/, "") + "/faucet");
// ===========================================================================

const CHAIN_ID_HEX = "0x" + CONFIG.CHAIN_ID.toString(16);
const CREATED_TOPIC = "0x4a1c716cc2323435ec5a77a7556c84da77d1ca36f6bd248bdd6e398a18ffe14b"; // Created(address,address,string,string,bytes)
const SEL = { name: "0x06fdde03", symbol: "0x95d89b41", reserveNative: "0xbf36b536", balanceOf: "0x70a08231",
  virtualNative: "0xff490386", curveTokens: "0x0d93caf7", totalSupply: "0x18160ddd" };

let account = null;

function eth(method, params = []) {
  if (!window.ethereum) return Promise.reject(new Error("No wallet found — install MetaMask."));
  return window.ethereum.request({ method, params });
}
const short = (a) => a ? a.slice(0, 6) + "…" + a.slice(-4) : "";
const esc = (s) => (s || "").replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const padL = (h, n = 64) => h.replace(/^0x/, "").padStart(n, "0");
const uint = (bi) => padL(bi.toString(16));
const toWei = (n) => BigInt(Math.floor(Number(n) * 1e6)) * (10n ** 12n); // 6-dp precision * 1e12
const fromWei = (bi) => Number(bi / (10n ** 14n)) / 1e4;
// compact LXS display: thousands for big market caps, significant digits for tiny per-token prices
function fmtLxs(bi) {
  const n = Number(bi) / 1e18;
  if (n === 0) return "0";
  if (n >= 1000) return Math.round(n).toLocaleString("en-US");
  if (n >= 1) return n.toFixed(2);
  return n.toPrecision(3);
}

function encBytes(bytes) {
  let hex = ""; for (const x of bytes) hex += x.toString(16).padStart(2, "0");
  if (hex.length % 64 !== 0) hex = hex.padEnd(hex.length + (64 - (hex.length % 64)), "0");
  return uint(BigInt(bytes.length)) + hex;
}
function encString(s) { return encBytes(new TextEncoder().encode(s)); }
function decodeString(hex) {
  hex = (hex || "").replace(/^0x/, "");
  if (hex.length < 128) return "";
  const len = parseInt(hex.slice(64, 128), 16);
  try { return new TextDecoder().decode(new Uint8Array(((hex.slice(128, 128 + len * 2)).match(/../g) || []).map(h => parseInt(h, 16)))); }
  catch { return ""; }
}
const ethCall = (to, data) => eth("eth_call", [{ to, data }, "latest"]);
const addrFromWord = (w) => "0x" + w.replace(/^0x/, "").slice(24);

// ---------- calldata ----------
function createCalldata(name, symbol, imgBytes, minTokensOut) {
  const n = encString(name), s = encString(symbol), img = encBytes(imgBytes || new Uint8Array());
  const off = (h) => uint(BigInt(h));
  const nl = n.length / 2, sl = s.length / 2;
  // four head words: offsets to name, symbol, image, then the static minTokensOut (head is 0x80)
  const head = off(0x80) + off(0x80 + nl) + off(0x80 + nl + sl) + uint(BigInt(minTokensOut || 0));
  return "0xdf5c2a2e" + head + n + s + img;                 // create(string,string,bytes,uint256)
}
const buyCalldata = () => "0xd96a094a" + uint(0n);           // buy(minTokensOut=0)
const sellCalldata = (amtWei) => "0xd79875eb" + uint(amtWei) + uint(0n); // sell(amount, minNativeOut=0)

// ---------- coin image (a thumbnail carried in the Created event, no off-chain host) ----------
// Resize any uploaded picture to a small square JPEG so it fits under the 12 KB on-chain cap.
async function fileToThumb(file) {
  if (!file) return new Uint8Array();
  const img = await new Promise((res, rej) => { const i = new Image(); i.onload = () => res(i); i.onerror = rej; i.src = URL.createObjectURL(file); });
  const S = 96, c = document.createElement("canvas"); c.width = S; c.height = S;
  const ctx = c.getContext("2d");
  const scale = Math.max(S / img.width, S / img.height);
  const w = img.width * scale, h = img.height * scale;
  ctx.drawImage(img, (S - w) / 2, (S - h) / 2, w, h);
  for (const q of [0.8, 0.6, 0.4]) {
    const blob = await new Promise(r => c.toBlob(r, "image/jpeg", q));
    const buf = new Uint8Array(await blob.arrayBuffer());
    if (buf.length <= 12288) return buf;            // under the on-chain cap
  }
  return new Uint8Array();                          // too big even at low quality — skip rather than revert
}
// Decode the JPEG thumbnail from a Created log's data into a data URL (or null if none).
function imageFromLog(dataHex) {
  const d = (dataHex || "").replace(/^0x/, "");
  const off = parseInt(d.slice(192, 256) || "0", 16) * 2;   // word3 = byte offset to the image
  if (!off || off + 64 > d.length) return null;
  const len = parseInt(d.slice(off, off + 64), 16);
  if (!len) return null;
  const bytesHex = d.slice(off + 64, off + 64 + len * 2);
  if (bytesHex.length < len * 2) return null;
  return "data:image/jpeg;base64," + hexToB64(bytesHex);
}
function hexToB64(hex) {
  let bin = ""; for (let i = 0; i < hex.length; i += 2) bin += String.fromCharCode(parseInt(hex.slice(i, i + 2), 16));
  return btoa(bin);
}

// ---------- wallet ----------
async function connect() {
  try {
    account = (await eth("eth_requestAccounts"))[0];
    document.getElementById("connect").textContent = short(account);
    document.getElementById("create").disabled = false;
    document.getElementById("create").textContent = "Create token";
    document.getElementById("wallet").style.display = "";
    document.getElementById("wAddr").textContent = account;
    refreshBalance();
    loadCoins();
  } catch (e) { alert(e.message || e); }
}

// ---------- native LXS: balance + send ----------
async function refreshBalance() {
  if (!account) return;
  try {
    const bal = BigInt(await eth("eth_getBalance", [account, "latest"]) || "0x0");
    document.getElementById("wBal").textContent = fromWei(bal).toLocaleString() + " LXS";
  } catch { /* leave the last shown value */ }
}
// Get a little LXS for gas from the node's faucet — so a brand-new wallet (0 balance)
// can pay for its first action (e.g. creating a token). One claim per address.
async function getGas() {
  if (!account) return;
  const msg = document.getElementById("gasMsg");
  msg.className = "msg"; msg.textContent = "Requesting gas…";
  try {
    const res = await fetch(FAUCET_URL, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ address: account }),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || ("HTTP " + res.status));
    msg.className = "msg ok"; msg.textContent = "Gas sent — refreshing balance…";
    setTimeout(refreshBalance, 4000);
  } catch (e) { msg.className = "msg err"; msg.textContent = "Faucet: " + (e.message || e); }
}
async function sendLXS() {
  const to = document.getElementById("sendTo").value.trim();
  const amt = document.getElementById("sendAmt").value;
  const msg = document.getElementById("sendMsg");
  if (!/^0x[0-9a-fA-F]{40}$/.test(to)) { msg.className = "msg err"; msg.textContent = "Enter a valid 0x… address."; return; }
  if (!amt || Number(amt) <= 0) { msg.className = "msg err"; msg.textContent = "Enter an LXS amount to send."; return; }
  msg.className = "msg"; msg.textContent = "Confirm in your wallet…";
  try {
    const tx = await eth("eth_sendTransaction", [{ from: account, to, value: "0x" + toWei(amt).toString(16), gas: "0x5208" }]);
    msg.className = "msg ok"; msg.textContent = "Sent! " + short(tx);
    document.getElementById("sendAmt").value = "";
    setTimeout(refreshBalance, 1500);
  } catch (e) { msg.className = "msg err"; msg.textContent = e.message || String(e); }
}
async function addNetwork() {
  try {
    await eth("wallet_addEthereumChain", [{
      chainId: CHAIN_ID_HEX, chainName: "LXS", rpcUrls: [CONFIG.RPC_URL],
      nativeCurrency: { name: "LXS", symbol: "LXS", decimals: 18 },
    }]);
  } catch (e) { alert(e.message || e); }
}

// ---------- create ----------
async function createCoin() {
  const name = document.getElementById("name").value.trim();
  const symbol = document.getElementById("symbol").value.trim();
  const msg = document.getElementById("createMsg");
  if (!CONFIG.FACTORY_ADDRESS) { msg.className = "msg err"; msg.textContent = "FACTORY_ADDRESS is not set in app.js — deploy the PumpFactory first."; return; }
  if (!name || !symbol) { msg.className = "msg err"; msg.textContent = "Fill in a name and symbol."; return; }
  msg.className = "msg"; msg.textContent = "Preparing image…";
  try {
    const img = await fileToThumb(document.getElementById("image").files[0]);
    // optional dev-buy: seed the coin with your own first buy IN THE SAME TX, so no sniper
    // can take the opening price between create and the first buy.
    const devBuy = document.getElementById("devbuy").value;
    const value = devBuy && Number(devBuy) > 0 ? "0x" + toWei(devBuy).toString(16) : "0x0";
    msg.textContent = "Confirm in your wallet…";
    // 1.3M covers a plain create (~1.02M–1.17M); a create+buy needs more, so raise the limit then
    const gas = value === "0x0" ? "0x13D620" : "0x1E8480";
    const tx = await eth("eth_sendTransaction", [{ from: account, to: CONFIG.FACTORY_ADDRESS, data: createCalldata(name, symbol, img, 0), value, gas }]);
    msg.className = "msg ok"; msg.textContent = "Token created! " + short(tx) + " — it's now live and tradeable.";
    setTimeout(loadCoins, 1500);
  } catch (e) { msg.className = "msg err"; msg.textContent = e.message || String(e); }
}

// ---------- deterministic coin icon (generated from the address; no storage, no upload) ----------
function mulberry32(seed) {
  return function () {
    seed |= 0; seed = (seed + 0x6D2B79F5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
// A unique 5x5 mirrored identicon per coin — same address always yields the same
// icon, so every viewer sees the same picture with zero shared storage.
function coinIcon(addr) {
  const h = (addr || "0x0").replace(/^0x/, "").toLowerCase().padStart(8, "0");
  const rnd = mulberry32(parseInt(h.slice(0, 8), 16) >>> 0);
  const hue = Math.floor(rnd() * 360);
  const fg = `hsl(${hue},65%,56%)`;
  const spot = `hsl(${(hue + 70) % 360},72%,62%)`;
  const bg = `hsl(${hue},26%,13%)`;
  let cells = "";
  for (let y = 0; y < 5; y++) for (let x = 0; x < 3; x++) {
    if (rnd() > 0.5) {
      const c = rnd() > 0.85 ? spot : fg;
      cells += `<rect x="${x}" y="${y}" width="1.03" height="1.03" fill="${c}"/>`;
      if (x < 2) cells += `<rect x="${4 - x}" y="${y}" width="1.03" height="1.03" fill="${c}"/>`;
    }
  }
  return `<svg class="ic" viewBox="0 0 5 5" preserveAspectRatio="xMidYMid meet" style="background:${bg}" xmlns="http://www.w3.org/2000/svg">${cells}</svg>`;
}

// ---------- list + trade ----------
// Reads the curve state that prices a coin. price/market-cap come straight from the curve:
// eff = virtualNative + reserveNative; price = eff/curveTokens; marketCap = price * totalSupply.
async function curveStats(coin) {
  const [nm, sym, reserve, vnat, ctok, tsup, bal] = await Promise.all([
    ethCall(coin, SEL.name).then(decodeString),
    ethCall(coin, SEL.symbol).then(decodeString),
    ethCall(coin, SEL.reserveNative).then(h => BigInt(h || "0x0")),
    ethCall(coin, SEL.virtualNative).then(h => BigInt(h || "0x0")),
    ethCall(coin, SEL.curveTokens).then(h => BigInt(h || "0x0")),
    ethCall(coin, SEL.totalSupply).then(h => BigInt(h || "0x0")),
    account ? ethCall(coin, SEL.balanceOf + padL(account)).then(h => BigInt(h || "0x0")) : Promise.resolve(0n),
  ]);
  const eff = vnat + reserve;
  const price = ctok > 0n ? (eff * (10n ** 18n)) / ctok : 0n;   // LXS-wei per 1 token
  const mc = ctok > 0n ? (eff * tsup) / ctok : 0n;              // market cap in LXS-wei
  const sold = tsup > ctok ? tsup - ctok : 0n;
  const progress = tsup > 0n ? Number((sold * 10000n) / tsup) / 100 : 0;
  return { coin, nm, sym, reserve, price, mc, bal, progress };
}

async function loadCoins() {
  const box = document.getElementById("coins");
  if (!CONFIG.FACTORY_ADDRESS) { box.innerHTML = '<p class="dim">Set FACTORY_ADDRESS in app.js to load tokens.</p>'; return; }
  box.innerHTML = '<p class="dim">Loading…</p>';
  try {
    const latest = parseInt(await eth("eth_blockNumber"), 16);
    const from = Math.max(0, latest - 9000);
    const logs = await eth("eth_getLogs", [{
      address: CONFIG.FACTORY_ADDRESS, fromBlock: "0x" + from.toString(16), toBlock: "latest",
      topics: [CREATED_TOPIC],
    }]);
    if (!logs.length) { box.innerHTML = '<p class="dim">No tokens yet — launch the first one.</p>'; return; }
    // first data word = coin address; the thumbnail rides later in the same log
    const coins = logs.map(lg => ({ addr: addrFromWord(lg.data.slice(0, 66)), img: imageFromLog(lg.data) }));
    // fetch every coin's curve state, then rank by market cap so the ones with real buying float to the top
    const stats = (await Promise.all(coins.map(async c => {
      const st = await curveStats(c.addr).catch(() => null);
      if (st) st.img = c.img;
      return st;
    }))).filter(Boolean);
    stats.sort((a, b) => (a.mc < b.mc ? 1 : a.mc > b.mc ? -1 : 0));
    box.innerHTML = "";
    for (const st of stats) renderCoin(box, st);
  } catch (e) { box.innerHTML = `<p class="msg err">${esc(e.message || String(e))}</p>`; }
}

function renderCoin(box, st) {
  const { coin, nm, sym, reserve, price, mc, bal, progress } = st;
  const card = document.createElement("div"); card.className = "coin";
  const pct = Math.min(100, Math.max(0, progress)).toFixed(1);
  card.innerHTML =
    `<div class="chead">${st.img ? `<img class="ic" src="${st.img}" alt="" onerror="this.style.visibility='hidden'">` : coinIcon(coin)}<div class="cmeta"><div class="sym">${esc(sym) || "?"}</div><div class="nm">${esc(nm) || ""}</div></div>
       <div class="mc">MC ${fmtLxs(mc)} LXS</div></div>
     <div class="bar"><span style="width:${pct}%"></span></div>
     <div class="sup">price ${fmtLxs(price)} · liq ${fromWei(reserve)} LXS · ${pct}% sold · you ${fromWei(bal)} ${esc(sym)}</div>
     <div class="addr">${coin}</div>
     <div class="trade">
       <input class="buyAmt" type="number" placeholder="LXS" step="0.01" min="0">
       <button class="buy">Buy</button>
       <input class="sellAmt" type="number" placeholder="${esc(sym)}" step="1" min="0">
       <button class="sell ghost">Sell</button>
     </div>`;
  card.querySelector(".buy").onclick = () => buy(coin, card.querySelector(".buyAmt").value);
  card.querySelector(".sell").onclick = () => sell(coin, card.querySelector(".sellAmt").value);
  box.appendChild(card);
}

async function buy(coin, lxs) {
  if (!lxs || Number(lxs) <= 0) return alert("Enter an LXS amount to spend.");
  try {
    await eth("eth_sendTransaction", [{ from: account, to: coin, data: buyCalldata(), value: "0x" + toWei(lxs).toString(16), gas: "0x30D40" }]);
    setTimeout(loadCoins, 1500); setTimeout(refreshBalance, 1500);
  } catch (e) { alert(e.message || e); }
}
async function sell(coin, amount) {
  if (!amount || Number(amount) <= 0) return alert("Enter a token amount to sell.");
  try {
    await eth("eth_sendTransaction", [{ from: account, to: coin, data: sellCalldata(toWei(amount)), gas: "0x30D40" }]);
    setTimeout(loadCoins, 1500); setTimeout(refreshBalance, 1500);
  } catch (e) { alert(e.message || e); }
}

// ---------- wire ----------
document.getElementById("connect").onclick = connect;
document.getElementById("addNet").onclick = addNetwork;
document.getElementById("create").onclick = createCoin;
document.getElementById("refresh").onclick = loadCoins;
document.getElementById("sendBtn").onclick = sendLXS;
document.getElementById("getGas").onclick = getGas;
if (window.ethereum && window.ethereum.selectedAddress) connect();
// keep the balance fresh while the page is open
setInterval(refreshBalance, 15000);
