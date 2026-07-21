// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

// Graduation — takes a launchpad coin to a Uniswap pool on Base so it can reach
// external DEXs / Coinbase, WITHOUT touching the immutable LXS core. Two contracts:
//
//   - GraduationVault (on LXS): the creator commits native LXS (>= minLiquidity)
//     plus a chunk of their coin. Both lock here as backing and a Graduated event
//     tells the off-chain operator to build the Base-side pool. The commitment gate
//     is on-chain: a graduation with no real liquidity behind it simply reverts.
//
//   - WrappedToken (on Base): a generalized operator-minted ERC-20 (name/symbol per
//     coin) that represents the graduated coin on Base. The operator mints it against
//     the coin locked in the vault, mints wLXS against the locked LXS, then seeds a
//     Uniswap wToken/wLXS pool. Coinbase's DEX indexer (0x/1inch) picks the pool up.
//
// Same custodial trust model as the LXS<->Base peg: mint only what is locked, a
// per-transfer nonce so a relayer restart can never double-mint, and every release
// is operator-only, nonce-once, and bounded by the real reserve.

interface IERC20 {
    function transferFrom(address from, address to, uint256 value) external returns (bool);
    function transfer(address to, uint256 value) external returns (bool);
    function balanceOf(address who) external view returns (uint256);
}

contract WrappedToken {
    string public name;
    string public symbol;
    uint8 public constant decimals = 18;

    address public immutable operator;
    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    // Per-transfer nonce: each graduation mint carries the vault's Graduated nonce;
    // mint records it and refuses a repeat, so a relayer that restarts or double-submits
    // can never mint the same graduation twice.
    mapping(uint256 => bool) public mintedNonce;
    uint256 public redeemNonce;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);
    event Redeem(uint256 indexed nonce, address indexed from, uint256 amount);

    constructor(address _operator, string memory _name, string memory _symbol) {
        require(_operator != address(0), "operator=0");
        operator = _operator;
        name = _name;
        symbol = _symbol;
    }

    // mint is operator-only: a wrapped token may exist only against the coin locked in
    // the vault on LXS. A used nonce is rejected, so double-minting a graduation is
    // impossible regardless of relayer bugs or restarts.
    function mint(uint256 nonce, address to, uint256 amount) external {
        require(msg.sender == operator, "not operator");
        require(to != address(0), "to=0");
        require(!mintedNonce[nonce], "nonce used");
        mintedNonce[nonce] = true;
        totalSupply += amount;
        balanceOf[to] += amount;
        emit Transfer(address(0), to, amount);
    }

    // redeem burns the caller's tokens and signals the operator to release the same coin
    // from the vault on LXS. Burning first keeps totalSupply <= locked backing at all times.
    function redeem(uint256 amount) external {
        uint256 bal = balanceOf[msg.sender];
        require(bal >= amount, "insufficient");
        balanceOf[msg.sender] = bal - amount;
        totalSupply -= amount;
        emit Transfer(msg.sender, address(0), amount);
        emit Redeem(redeemNonce++, msg.sender, amount);
    }

    function transfer(address to, uint256 value) external returns (bool) {
        return _transfer(msg.sender, to, value);
    }
    function transferFrom(address from, address to, uint256 value) external returns (bool) {
        uint256 a = allowance[from][msg.sender];
        require(a >= value, "allowance");
        if (a != type(uint256).max) allowance[from][msg.sender] = a - value;
        return _transfer(from, to, value);
    }
    function approve(address spender, uint256 value) external returns (bool) {
        allowance[msg.sender][spender] = value;
        emit Approval(msg.sender, spender, value);
        return true;
    }
    function _transfer(address from, address to, uint256 value) internal returns (bool) {
        require(to != address(0), "to=0");
        uint256 b = balanceOf[from];
        require(b >= value, "balance");
        balanceOf[from] = b - value;
        balanceOf[to] += value;
        emit Transfer(from, to, value);
        return true;
    }
}

contract GraduationVault {
    address public immutable operator;
    uint256 public immutable minLiquidity; // the "at least ~1 pound of LXS" gate, in wei
    bool private entered;

    uint256 public gradNonce;
    mapping(uint256 => bool) public releasedLxsNonce;
    mapping(uint256 => bool) public releasedTokenNonce;

    event Graduated(uint256 indexed nonce, address indexed coin, address indexed from, uint256 lxsAmount, uint256 tokenAmount);
    event ReleasedLxs(uint256 indexed nonce, address indexed to, uint256 amount);
    event ReleasedToken(uint256 indexed nonce, address indexed coin, address indexed to, uint256 amount);

    modifier noReentry() { require(!entered, "reentrant"); entered = true; _; entered = false; }

    constructor(address _operator, uint256 _minLiquidity) {
        require(_operator != address(0), "operator=0");
        require(_minLiquidity > 0, "minLiquidity=0");
        operator = _operator;
        minLiquidity = _minLiquidity;
    }

    // graduate commits msg.value native LXS (>= minLiquidity) and tokenAmount of `coin`
    // (the caller must approve the vault first). Both lock here as backing; the Graduated
    // event tells the operator to mint wLXS + WrappedToken on Base and seed the pool. It
    // reverts if the LXS commitment is below the gate or the token pull fails, so a
    // graduation without real committed liquidity cannot be recorded.
    function graduate(address coin, uint256 tokenAmount) external payable noReentry {
        require(msg.value >= minLiquidity, "below min liquidity");
        require(tokenAmount > 0, "token=0");
        require(coin != address(0), "coin=0");
        require(IERC20(coin).transferFrom(msg.sender, address(this), tokenAmount), "token pull failed");
        emit Graduated(gradNonce++, coin, msg.sender, msg.value, tokenAmount);
    }

    function lxsReserve() external view returns (uint256) { return address(this).balance; }
    function tokenReserve(address coin) external view returns (uint256) { return IERC20(coin).balanceOf(address(this)); }

    // releaseLxs / releaseToken unwind a graduation (operator-only, nonce-once, never
    // over the real reserve) — the same bounded-operator safety as the peg vault, for a
    // reversed graduation or an unwound pool. Effects are written before the external
    // call (checks-effects-interactions) on top of the reentrancy guard.
    function releaseLxs(uint256 nonce, address to, uint256 amount) external noReentry {
        require(msg.sender == operator, "not operator");
        require(to != address(0), "to=0");
        require(!releasedLxsNonce[nonce], "nonce used");
        require(amount <= address(this).balance, "over reserve");
        releasedLxsNonce[nonce] = true;
        (bool ok, ) = to.call{value: amount}("");
        require(ok, "send failed");
        emit ReleasedLxs(nonce, to, amount);
    }
    function releaseToken(uint256 nonce, address coin, address to, uint256 amount) external noReentry {
        require(msg.sender == operator, "not operator");
        require(to != address(0), "to=0");
        require(!releasedTokenNonce[nonce], "nonce used");
        releasedTokenNonce[nonce] = true;
        require(IERC20(coin).transfer(to, amount), "token send failed");
        emit ReleasedToken(nonce, coin, to, amount);
    }
}
