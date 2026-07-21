// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

// The LXS<->Base custodial peg — two contracts:
//   - PegVault (on LXS): locks native LXS as backing, releases it on redeem.
//   - WrappedLXS (on Base): an ERC-20 the operator mints against locked LXS;
//     anyone can redeem (burn) to get native LXS back on the LXS side.
//
// Custodial by design: an off-chain operator watches Locked/Redeem events and
// mirrors them across the two chains. The invariant reserve() on LXS >=
// wLXS.totalSupply() on Base is a cross-chain property the operator maintains
// (mint only what is locked, release only what is redeemed). Each contract still
// enforces local safety to bound a buggy operator: only the operator
// mints/releases, a release never exceeds the real reserve, a redeemer can only
// burn tokens it holds, and redeem burns before the operator releases so supply
// never exceeds reserve.

contract WrappedLXS {
    // Display name/symbol are plain "LXS": on Base DEXs and wallets it shows as
    // LXS, not wLXS. It is still the bridged token internally (see the header);
    // only the public name()/symbol() strings say LXS.
    string public constant name = "LXS";
    string public constant symbol = "LXS";
    uint8 public constant decimals = 18;

    address public immutable operator;
    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    // Idempotency (the ChainBridge pattern): each Locked event on the LXS side carries
    // a unique nonce; mint records it and refuses a repeat, so a relayer that restarts
    // or double-submits can NEVER double-mint the same lock. redeemNonce numbers the
    // reverse direction the same way.
    mapping(uint256 => bool) public mintedNonce;
    uint256 public redeemNonce;

    // ERC-1046 (tokenURI Interoperability): a data: URI whose JSON carries name/symbol/
    // image (the LXS logo). Wallets like MetaMask read it via wallet_watchAsset and show
    // the logo with no external host and no token-list submission. Set ONCE by the operator
    // after deploy (the chosen logo is plugged in then) and then frozen, so the branding
    // cannot be changed under holders afterwards.
    string public tokenURI;
    bool private uriFrozen;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);
    // The operator watches Redeem on Base and releases native LXS on the LXS side.
    event Redeem(uint256 indexed nonce, address indexed from, uint256 amount);

    constructor(address _operator) {
        require(_operator != address(0), "operator=0");
        operator = _operator;
    }

    // setTokenURI installs the logo/metadata once. Operator-only and one-shot: after the
    // first call it is frozen, so the public branding is fixed like the rest of the token.
    function setTokenURI(string calldata uri) external {
        require(msg.sender == operator, "not operator");
        require(!uriFrozen, "frozen");
        uriFrozen = true;
        tokenURI = uri;
    }

    // mint is operator-only: wLXS may exist only against LXS locked in the vault.
    // nonce is the Locked event's nonce; a repeat is rejected, so double-minting a
    // lock is impossible regardless of relayer bugs or restarts.
    function mint(uint256 nonce, address to, uint256 amount) external {
        require(msg.sender == operator, "not operator");
        require(to != address(0), "to=0");
        require(!mintedNonce[nonce], "nonce used");
        mintedNonce[nonce] = true;
        totalSupply += amount;
        balanceOf[to] += amount;
        emit Transfer(address(0), to, amount);
    }

    // redeem burns the caller's wLXS and signals the operator to release the same
    // native LXS. Burning first keeps totalSupply <= reserve at all times. Each redeem
    // gets a fresh nonce the vault uses to release exactly once.
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

contract PegVault {
    address public immutable operator;
    bool private entered;

    // lockNonce numbers each lock so the Base side mints it exactly once; releasedNonce
    // records which redeem nonces have already been released, so a relayer restart or
    // double-submit can never release the same redeem twice.
    uint256 public lockNonce;
    mapping(uint256 => bool) public releasedNonce;

    event Locked(uint256 indexed nonce, address indexed from, uint256 amount);
    event Released(uint256 indexed nonce, address indexed to, uint256 amount);

    modifier noReentry() { require(!entered, "reentrant"); entered = true; _; entered = false; }

    constructor(address _operator) {
        require(_operator != address(0), "operator=0");
        operator = _operator;
    }

    // lock deposits native LXS as backing. The operator watches Locked and mints the
    // same amount of wLXS to the sender on Base, keyed by the emitted nonce.
    function lock() external payable {
        require(msg.value > 0, "zero");
        emit Locked(lockNonce++, msg.sender, msg.value);
    }
    // a bare send also locks (convenience), so a plain transfer to the vault is backed.
    receive() external payable {
        require(msg.value > 0, "zero");
        emit Locked(lockNonce++, msg.sender, msg.value);
    }

    // reserve is the real native LXS backing the wLXS supply. The peg is healthy while
    // reserve() (here) >= wLXS.totalSupply() (on Base) — kept true by the operator.
    function reserve() external view returns (uint256) {
        return address(this).balance;
    }

    // release returns locked LXS to a redeemer. Operator-only, never more than the
    // real reserve, so a compromised operator cannot release money that is not
    // there. The guard runs before the external send (reentrancy-safe).
    function release(uint256 nonce, address to, uint256 amount) external noReentry {
        require(msg.sender == operator, "not operator");
        require(to != address(0), "to=0");
        require(!releasedNonce[nonce], "nonce used");
        require(amount <= address(this).balance, "over reserve");
        releasedNonce[nonce] = true;
        (bool ok, ) = to.call{value: amount}("");
        require(ok, "send failed");
        emit Released(nonce, to, amount);
    }
}
