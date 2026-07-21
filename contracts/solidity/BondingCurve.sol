// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

// A bonding-curve coin: each coin is its own ERC-20 and its own market. A
// constant-product curve with virtual reserves sets the opening price, so no
// upfront liquidity is needed; real native LXS accumulates as people buy. buy()
// mints tokens out of the curve, sell() burns them back in. A trading fee goes to
// feeRecipient (an address, or the burn address for deflation).
//
// Virtual-reserve invariant (checked in tests): after a buy of X native, a full
// sell of the tokens received returns X minus fees and leaves the curve at its
// start, so the real reserve always covers every sell and the virtual reserve is
// never paid out. This is what lets the curve open with zero real liquidity and
// stay solvent.
contract PumpCoin {
    string public name;
    string public symbol;
    uint8 public constant decimals = 18;
    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);
    event Buy(address indexed who, uint256 nativeIn, uint256 tokensOut);
    event Sell(address indexed who, uint256 tokensIn, uint256 nativeOut);

    uint256 public reserveNative;            // real native the curve holds
    uint256 public immutable virtualNative;  // virtual reserve — sets the opening price
    uint256 public curveTokens;              // tokens the curve still has to sell
    address public immutable feeRecipient;
    uint256 public immutable feeBps;
    uint256 public feeAccrued;               // fees held for a pull withdrawal (never mixed with reserveNative)

    uint256 private unlocked = 1;
    modifier lock() {
        require(unlocked == 1, "PumpCoin: locked");
        unlocked = 0;
        _;
        unlocked = 1;
    }

    constructor(string memory _name, string memory _symbol, uint256 _curveSupply,
                uint256 _virtualNative, address _feeRecipient, uint256 _feeBps) {
        name = _name;
        symbol = _symbol;
        curveTokens = _curveSupply;
        virtualNative = _virtualNative;
        feeRecipient = _feeRecipient;
        feeBps = _feeBps;
    }

    function _mint(address to, uint256 v) internal {
        totalSupply += v;
        balanceOf[to] += v;
        emit Transfer(address(0), to, v);
    }
    function _burn(address from, uint256 v) internal {
        balanceOf[from] -= v;
        totalSupply -= v;
        emit Transfer(from, address(0), v);
    }

    // quoteBuy is the read-only price a UI shows before a buy.
    function quoteBuy(uint256 nativeIn) external view returns (uint256) {
        uint256 inAfterFee = nativeIn - (nativeIn * feeBps) / 10000;
        uint256 eff = virtualNative + reserveNative;
        return curveTokens - (eff * curveTokens) / (eff + inAfterFee);
    }

    // buy mints curve tokens to the caller. buyTo credits a chosen recipient, so the
    // factory can perform the creator's first buy atomically inside create() (an opening
    // buy in a separate tx can be front-run by a sniper at the bottom of the curve).
    function buy(uint256 minTokensOut) external payable returns (uint256) {
        return buyTo(msg.sender, minTokensOut);
    }

    function buyTo(address to, uint256 minTokensOut) public payable lock returns (uint256 out) {
        require(msg.value > 0, "PumpCoin: no value");
        require(to != address(0), "PumpCoin: to=0");
        uint256 fee = (msg.value * feeBps) / 10000;
        uint256 inAmt = msg.value - fee;
        uint256 eff = virtualNative + reserveNative;
        out = curveTokens - (eff * curveTokens) / (eff + inAmt);
        require(out >= minTokensOut, "PumpCoin: slippage");
        require(out <= curveTokens, "PumpCoin: curve empty");
        reserveNative += inAmt;
        curveTokens -= out;
        // Fee is ACCRUED, not pushed: a feeRecipient that reverts on receipt can never
        // block a buy/sell — only its own withdrawFees() fails. (Pull over push.)
        feeAccrued += fee;
        _mint(to, out);
        emit Buy(to, msg.value, out);
    }

    // withdrawFees pushes the accrued fees to feeRecipient. Permissionless (anyone may
    // trigger it) and isolated from trading, so a bad feeRecipient bricks nothing else.
    function withdrawFees() external {
        uint256 f = feeAccrued;
        feeAccrued = 0;
        if (f > 0) {
            (bool ok, ) = payable(feeRecipient).call{value: f}("");
            require(ok, "PumpCoin: fee send");
        }
    }

    function sell(uint256 amount, uint256 minNativeOut) external lock {
        require(amount > 0 && balanceOf[msg.sender] >= amount, "PumpCoin: balance");
        uint256 eff = virtualNative + reserveNative;
        // reverse constant product: returning `amount` tokens releases grossOut.
        uint256 grossOut = eff - (eff * curveTokens) / (curveTokens + amount);
        // Integer division can leave grossOut a few wei above the real reserve
        // (the virtual reserve is not real money). Clamp to what the curve holds:
        // the dust stays with the pool and the last seller exits with the full
        // reserve, keeping the curve solvent.
        if (grossOut > reserveNative) grossOut = reserveNative;
        uint256 fee = (grossOut * feeBps) / 10000;
        uint256 out = grossOut - fee;
        require(out >= minNativeOut, "PumpCoin: slippage");
        // EFFECTS before INTERACTION (plus the lock): update the curve first. The fee is
        // accrued for a pull withdrawal, so the only external call is the seller's payout.
        curveTokens += amount;
        reserveNative -= grossOut;
        feeAccrued += fee;
        _burn(msg.sender, amount);
        (bool ok, ) = payable(msg.sender).call{value: out}("");
        require(ok, "PumpCoin: native send");
        emit Sell(msg.sender, amount, out);
    }

    function transfer(address to, uint256 v) external returns (bool) {
        _transfer(msg.sender, to, v);
        return true;
    }
    function approve(address spender, uint256 v) external returns (bool) {
        allowance[msg.sender][spender] = v;
        emit Approval(msg.sender, spender, v);
        return true;
    }
    function transferFrom(address from, address to, uint256 v) external returns (bool) {
        uint256 a = allowance[from][msg.sender];
        require(a >= v, "PumpCoin: allowance");
        if (a != type(uint256).max) allowance[from][msg.sender] = a - v;
        _transfer(from, to, v);
        return true;
    }
    function _transfer(address from, address to, uint256 v) internal {
        require(balanceOf[from] >= v, "PumpCoin: balance");
        balanceOf[from] -= v;
        balanceOf[to] += v;
        emit Transfer(from, to, v);
    }
}

// PumpFactory spins up a coin+curve in one transaction. Curve parameters and the
// platform fee are fixed here, so every coin trades on the same terms.
contract PumpFactory {
    // image is a small thumbnail (the creator's coin photo) carried in the event log, not
    // in contract storage — the site reads it straight from Created via eth_getLogs, so no
    // off-chain host, no IPFS, no backend. It is capped so a coin cannot bloat the log with
    // a huge blob; empty is allowed (the site falls back to a generated identicon).
    event Created(address indexed creator, address coin, string name, string symbol, bytes image);

    // Curve defaults: 800M tokens along the curve, a 30-LXS virtual reserve for a
    // low opening price. These affect only the price path, not solvency.
    uint256 public constant CURVE_SUPPLY = 800_000_000 ether;
    uint256 public constant VIRTUAL_NATIVE = 30 ether;
    uint256 public constant MAX_IMAGE = 12_288; // 12 KB cap on the embedded thumbnail

    address public immutable feeRecipient;
    uint256 public immutable feeBps;

    constructor(address _feeRecipient, uint256 _feeBps) {
        require(_feeBps <= 1000, "PumpFactory: fee too high"); // <= 10%
        feeRecipient = _feeRecipient;
        feeBps = _feeBps;
    }

    // create spins up the coin and, if native is sent, performs the creator's first buy in
    // the SAME tx (crediting msg.sender) so no sniper can take the opening price between the
    // create and the first buy. minTokensOut bounds that buy's slippage; pass 0 to skip the
    // buy (create with no initial liquidity, as before).
    function create(string calldata name, string calldata symbol, bytes calldata image, uint256 minTokensOut)
        external payable returns (address)
    {
        require(image.length <= MAX_IMAGE, "PumpFactory: image too big");
        PumpCoin coin = new PumpCoin(name, symbol, CURVE_SUPPLY, VIRTUAL_NATIVE, feeRecipient, feeBps);
        if (msg.value > 0) {
            coin.buyTo{value: msg.value}(msg.sender, minTokensOut);
        }
        emit Created(msg.sender, address(coin), name, symbol, image);
        return address(coin);
    }
}
