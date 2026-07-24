// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

// LxsSwap — a Uniswap-V2-compatible AMM native to LXS, so launchpad coins can live
// in a STANDARD constant-product pool (not just the bonding curve). This is the
// "DEX-market" a price aggregator (DexScreener/GeckoTerminal) reads: it discovers
// pairs from the factory's PairCreated event and reads each pair's token0/token1,
// getReserves(), and Sync/Swap events — the exact V2 ABI reproduced here. Nothing
// here touches the immutable LXS core; it is ordinary on-chain contracts.
//
// Why V2-exact and not a bespoke curve: an aggregator's generic EVM indexer matches
// on the V2 event topics and view selectors. Rename or reshape them and the pool
// stops being machine-readable — the whole point (external visibility) is lost. So
// the reserves are uint112, getReserves returns (uint112,uint112,uint32), and Swap/
// Sync/Mint/Burn/PairCreated carry the canonical signatures byte-for-byte.

interface IERC20 {
    function balanceOf(address) external view returns (uint256);
    function transfer(address to, uint256 value) external returns (bool);
    function transferFrom(address from, address to, uint256 value) external returns (bool);
}

// Canonical V2 flash-swap callback: only invoked when swap() is called with non-empty
// data, so ordinary swaps never touch it. Kept for drop-in tooling compatibility.
interface ILxsSwapCallee {
    function lxsSwapCall(address sender, uint256 amount0, uint256 amount1, bytes calldata data) external;
}

// A minimal ERC-20 the pair inherits for its LP token.
contract LxsSwapERC20 {
    string public constant name = "LxsSwap LP";
    string public constant symbol = "LXS-LP";
    uint8 public constant decimals = 18;
    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    function _mint(address to, uint256 value) internal {
        totalSupply += value;
        balanceOf[to] += value;
        emit Transfer(address(0), to, value);
    }

    function _burn(address from, uint256 value) internal {
        balanceOf[from] -= value;
        totalSupply -= value;
        emit Transfer(from, address(0), value);
    }

    function approve(address spender, uint256 value) external returns (bool) {
        allowance[msg.sender][spender] = value;
        emit Approval(msg.sender, spender, value);
        return true;
    }

    function transfer(address to, uint256 value) external returns (bool) {
        _transfer(msg.sender, to, value);
        return true;
    }

    function transferFrom(address from, address to, uint256 value) external returns (bool) {
        uint256 a = allowance[from][msg.sender];
        if (a != type(uint256).max) allowance[from][msg.sender] = a - value;
        _transfer(from, to, value);
        return true;
    }

    function _transfer(address from, address to, uint256 value) private {
        balanceOf[from] -= value;
        balanceOf[to] += value;
        emit Transfer(from, to, value);
    }
}

// WLXS — WETH-style wrapper for native LXS. deposit() to wrap, withdraw() to unwrap,
// 1:1 backed always. A V2 pair is ERC20/ERC20, so native LXS must be wrapped to be
// one side of a pair. This is the shared base asset: every launchpad pool is
// COIN/WLXS, so buying any graduated coin still routes demand through LXS.
contract WLXS {
    string public constant name = "Wrapped LXS";
    string public constant symbol = "WLXS";
    uint8 public constant decimals = 18;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    event Approval(address indexed owner, address indexed spender, uint256 value);
    event Transfer(address indexed from, address indexed to, uint256 value);
    event Deposit(address indexed dst, uint256 value);
    event Withdrawal(address indexed src, uint256 value);

    receive() external payable { deposit(); }

    function deposit() public payable {
        balanceOf[msg.sender] += msg.value;
        emit Deposit(msg.sender, msg.value);
    }

    // withdraw burns WLXS and returns the native LXS. Loud on a failed send: a silent
    // failure would let a caller think it unwrapped while the LXS stayed locked.
    function withdraw(uint256 value) public {
        require(balanceOf[msg.sender] >= value, "WLXS: balance");
        balanceOf[msg.sender] -= value;
        (bool ok, ) = msg.sender.call{value: value}("");
        require(ok, "WLXS: LXS send failed");
        emit Withdrawal(msg.sender, value);
    }

    function totalSupply() public view returns (uint256) { return address(this).balance; }

    function approve(address spender, uint256 value) public returns (bool) {
        allowance[msg.sender][spender] = value;
        emit Approval(msg.sender, spender, value);
        return true;
    }

    function transfer(address to, uint256 value) public returns (bool) {
        return transferFrom(msg.sender, to, value);
    }

    function transferFrom(address from, address to, uint256 value) public returns (bool) {
        require(balanceOf[from] >= value, "WLXS: balance");
        if (from != msg.sender && allowance[from][msg.sender] != type(uint256).max) {
            require(allowance[from][msg.sender] >= value, "WLXS: allowance");
            allowance[from][msg.sender] -= value;
        }
        balanceOf[from] -= value;
        balanceOf[to] += value;
        emit Transfer(from, to, value);
        return true;
    }
}

// The pair: constant-product x*y=k with a 0.30% fee, reserves as uint112, canonical
// V2 events and views. One deployed per token pair by the factory.
contract LxsSwapPair is LxsSwapERC20 {
    uint256 public constant MINIMUM_LIQUIDITY = 1000;

    address public factory;
    address public token0;
    address public token1;
    // If set, ONLY this address may perform the FIRST mint (until the pool has any LP).
    // A launchpad coin sets it to itself so no third party can pre-seed the pool before
    // graduation and skim the graduation liquidity via an imbalanced first deposit. Once
    // seeded (totalSupply > 0) the gate is irrelevant and the pool is fully permissionless.
    address public graduator;

    uint112 private reserve0;
    uint112 private reserve1;
    uint32  private blockTimestampLast;

    uint256 public price0CumulativeLast;
    uint256 public price1CumulativeLast;

    uint256 private unlocked = 1;
    modifier lock() {
        require(unlocked == 1, "LxsSwap: LOCKED");
        unlocked = 0;
        _;
        unlocked = 1;
    }

    event Mint(address indexed sender, uint256 amount0, uint256 amount1);
    event Burn(address indexed sender, uint256 amount0, uint256 amount1, address indexed to);
    event Swap(
        address indexed sender,
        uint256 amount0In,
        uint256 amount1In,
        uint256 amount0Out,
        uint256 amount1Out,
        address indexed to
    );
    event Sync(uint112 reserve0, uint112 reserve1);

    constructor() { factory = msg.sender; }

    // Called once by the factory right after CREATE.
    function initialize(address _token0, address _token1, address _graduator) external {
        require(msg.sender == factory, "LxsSwap: FORBIDDEN");
        token0 = _token0;
        token1 = _token1;
        graduator = _graduator;
    }

    function getReserves() public view returns (uint112 _reserve0, uint112 _reserve1, uint32 _blockTimestampLast) {
        _reserve0 = reserve0;
        _reserve1 = reserve1;
        _blockTimestampLast = blockTimestampLast;
    }

    function _safeTransfer(address token, address to, uint256 value) private {
        (bool ok, bytes memory data) = token.call(abi.encodeWithSelector(0xa9059cbb, to, value));
        require(ok && (data.length == 0 || abi.decode(data, (bool))), "LxsSwap: TRANSFER_FAILED");
    }

    // _update writes the new reserves and advances the TWAP accumulators. The uint32
    // timestamp and the accumulator adds are meant to WRAP (V2 relies on it), so they
    // sit in `unchecked` — under 0.8 they would otherwise revert at the 32-bit / 256-bit
    // boundary and freeze the oracle every ~136 years / on natural overflow.
    function _update(uint256 balance0, uint256 balance1, uint112 _reserve0, uint112 _reserve1) private {
        require(balance0 <= type(uint112).max && balance1 <= type(uint112).max, "LxsSwap: OVERFLOW");
        unchecked {
            uint32 blockTimestamp = uint32(block.timestamp % 2**32);
            uint32 timeElapsed = blockTimestamp - blockTimestampLast;
            if (timeElapsed > 0 && _reserve0 != 0 && _reserve1 != 0) {
                price0CumulativeLast += (uint256(_reserve1) * (2**112) / _reserve0) * timeElapsed;
                price1CumulativeLast += (uint256(_reserve0) * (2**112) / _reserve1) * timeElapsed;
            }
            blockTimestampLast = blockTimestamp;
        }
        reserve0 = uint112(balance0);
        reserve1 = uint112(balance1);
        emit Sync(reserve0, reserve1);
    }

    // mint LP tokens for liquidity that was already transferred in (V2 style: the
    // caller sends both tokens, then calls mint). Graduation uses exactly this.
    function mint(address to) external lock returns (uint256 liquidity) {
        // Gate ONLY the first deposit: a pre-seed by anyone but the graduator would let
        // them skim the graduation liquidity through an imbalanced pool ratio.
        if (graduator != address(0) && totalSupply == 0) require(msg.sender == graduator, "LxsSwap: GATED");
        (uint112 _reserve0, uint112 _reserve1, ) = getReserves();
        uint256 balance0 = IERC20(token0).balanceOf(address(this));
        uint256 balance1 = IERC20(token1).balanceOf(address(this));
        uint256 amount0 = balance0 - _reserve0;
        uint256 amount1 = balance1 - _reserve1;

        uint256 _totalSupply = totalSupply;
        if (_totalSupply == 0) {
            liquidity = _sqrt(amount0 * amount1) - MINIMUM_LIQUIDITY;
            _mint(address(0), MINIMUM_LIQUIDITY); // lock the first units so the pool can never be fully drained
        } else {
            uint256 l0 = amount0 * _totalSupply / _reserve0;
            uint256 l1 = amount1 * _totalSupply / _reserve1;
            liquidity = l0 < l1 ? l0 : l1;
        }
        require(liquidity > 0, "LxsSwap: INSUFFICIENT_LIQUIDITY_MINTED");
        _mint(to, liquidity);
        _update(balance0, balance1, _reserve0, _reserve1);
        emit Mint(msg.sender, amount0, amount1);
    }

    function burn(address to) external lock returns (uint256 amount0, uint256 amount1) {
        uint256 balance0 = IERC20(token0).balanceOf(address(this));
        uint256 balance1 = IERC20(token1).balanceOf(address(this));
        uint256 liquidity = balanceOf[address(this)];

        uint256 _totalSupply = totalSupply;
        amount0 = liquidity * balance0 / _totalSupply;
        amount1 = liquidity * balance1 / _totalSupply;
        require(amount0 > 0 && amount1 > 0, "LxsSwap: INSUFFICIENT_LIQUIDITY_BURNED");
        _burn(address(this), liquidity);
        _safeTransfer(token0, to, amount0);
        _safeTransfer(token1, to, amount1);
        balance0 = IERC20(token0).balanceOf(address(this));
        balance1 = IERC20(token1).balanceOf(address(this));

        (uint112 _reserve0, uint112 _reserve1, ) = getReserves();
        _update(balance0, balance1, _reserve0, _reserve1);
        emit Burn(msg.sender, amount0, amount1, to);
    }

    // swap sends `amountXOut` optimistically then checks the K invariant held (0.3% fee):
    // the router computes the exact input, transfers it in, then calls swap.
    function swap(uint256 amount0Out, uint256 amount1Out, address to, bytes calldata data) external lock {
        require(amount0Out > 0 || amount1Out > 0, "LxsSwap: INSUFFICIENT_OUTPUT_AMOUNT");
        (uint112 _reserve0, uint112 _reserve1, ) = getReserves();
        require(amount0Out < _reserve0 && amount1Out < _reserve1, "LxsSwap: INSUFFICIENT_LIQUIDITY");

        uint256 balance0;
        uint256 balance1;
        { // scope for token{0,1} — keeps the stack shallow (V2's "stack too deep" fix)
            address _token0 = token0;
            address _token1 = token1;
            require(to != _token0 && to != _token1, "LxsSwap: INVALID_TO");
            if (amount0Out > 0) _safeTransfer(_token0, to, amount0Out);
            if (amount1Out > 0) _safeTransfer(_token1, to, amount1Out);
            if (data.length > 0) ILxsSwapCallee(to).lxsSwapCall(msg.sender, amount0Out, amount1Out, data);
            balance0 = IERC20(_token0).balanceOf(address(this));
            balance1 = IERC20(_token1).balanceOf(address(this));
        }
        uint256 amount0In = balance0 > _reserve0 - amount0Out ? balance0 - (_reserve0 - amount0Out) : 0;
        uint256 amount1In = balance1 > _reserve1 - amount1Out ? balance1 - (_reserve1 - amount1Out) : 0;
        require(amount0In > 0 || amount1In > 0, "LxsSwap: INSUFFICIENT_INPUT_AMOUNT");
        { // scope for balance*Adjusted — the 0.30% fee-preserving K check
            uint256 balance0Adjusted = balance0 * 1000 - amount0In * 3;
            uint256 balance1Adjusted = balance1 * 1000 - amount1In * 3;
            require(
                balance0Adjusted * balance1Adjusted >= uint256(_reserve0) * _reserve1 * (1000**2),
                "LxsSwap: K"
            );
        }
        _update(balance0, balance1, _reserve0, _reserve1);
        emit Swap(msg.sender, amount0In, amount1In, amount0Out, amount1Out, to);
    }

    // force reserves to match balances (recover from a donation/rounding drift).
    function sync() external lock {
        _update(
            IERC20(token0).balanceOf(address(this)),
            IERC20(token1).balanceOf(address(this)),
            reserve0,
            reserve1
        );
    }

    function _sqrt(uint256 y) private pure returns (uint256 z) {
        if (y > 3) {
            z = y;
            uint256 x = y / 2 + 1;
            while (x < z) {
                z = x;
                x = (y / x + x) / 2;
            }
        } else if (y != 0) {
            z = 1;
        }
    }
}

// The factory: creates one pair per token combination and lets an aggregator
// enumerate every market from PairCreated + allPairs.
contract LxsSwapFactory {
    address public feeTo;
    address public feeToSetter;

    mapping(address => mapping(address => address)) public getPair;
    address[] public allPairs;

    event PairCreated(address indexed token0, address indexed token1, address pair, uint256);

    constructor(address _feeToSetter) { feeToSetter = _feeToSetter; }

    function allPairsLength() external view returns (uint256) { return allPairs.length; }

    // Ordinary permissionless pair (no first-mint gate) — the general DEX path.
    function createPair(address tokenA, address tokenB) external returns (address pair) {
        return _createPair(tokenA, tokenB, address(0));
    }

    // Gated pair: the FIRST mint is restricted to `graduator`. To stop a griefer from
    // gating another token's pool to themselves, only the graduator itself may create it
    // (msg.sender == graduator), and the graduator must be one side of the pair. A
    // launchpad coin calls this for its own COIN/WLXS pool so no one can pre-seed it.
    function createPairGated(address tokenA, address tokenB, address graduator) external returns (address pair) {
        require(msg.sender == graduator, "LxsSwap: NOT_GRADUATOR");
        require(graduator == tokenA || graduator == tokenB, "LxsSwap: GRADUATOR_NOT_IN_PAIR");
        return _createPair(tokenA, tokenB, graduator);
    }

    function _createPair(address tokenA, address tokenB, address graduator) internal returns (address pair) {
        require(tokenA != tokenB, "LxsSwap: IDENTICAL_ADDRESSES");
        (address token0, address token1) = tokenA < tokenB ? (tokenA, tokenB) : (tokenB, tokenA);
        require(token0 != address(0), "LxsSwap: ZERO_ADDRESS");
        require(getPair[token0][token1] == address(0), "LxsSwap: PAIR_EXISTS");

        LxsSwapPair p = new LxsSwapPair();
        p.initialize(token0, token1, graduator);
        pair = address(p);

        getPair[token0][token1] = pair;
        getPair[token1][token0] = pair;
        allPairs.push(pair);
        emit PairCreated(token0, token1, pair, allPairs.length);
    }

    function setFeeTo(address _feeTo) external {
        require(msg.sender == feeToSetter, "LxsSwap: FORBIDDEN");
        feeTo = _feeTo;
    }

    function setFeeToSetter(address _feeToSetter) external {
        require(msg.sender == feeToSetter, "LxsSwap: FORBIDDEN");
        feeToSetter = _feeToSetter;
    }
}
