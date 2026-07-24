// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

// LxsSwapRouter — the periphery that makes trading an LxsSwap pool safe from a wallet:
// one atomic tx wraps LXS->WLXS, swaps, and (on a sell) unwraps back to native LXS,
// with a slippage floor and a deadline. Without it a browser would need three separate
// txs (deposit, transfer, swap) and a sandwich could land between them.
//
// Pairs are looked up via factory.getPair (the factory deploys pairs with CREATE, so
// there is no CREATE2 init-code-hash to compute). Kept minimal: the two swaps the
// launchpad UI needs, plus read-only quotes.

interface IWLXSr {
    function deposit() external payable;
    function withdraw(uint256) external;
    function transfer(address to, uint256 value) external returns (bool);
}
interface IFactoryr {
    function getPair(address a, address b) external view returns (address);
}
interface IPairr {
    function getReserves() external view returns (uint112, uint112, uint32);
    function swap(uint256 amount0Out, uint256 amount1Out, address to, bytes calldata data) external;
}
interface ITokenr {
    function transferFrom(address from, address to, uint256 value) external returns (bool);
}

contract LxsSwapRouter {
    address public immutable factory;
    address public immutable WLXS;

    constructor(address _factory, address _wlxs) {
        factory = _factory;
        WLXS = _wlxs;
    }

    // Only WLXS may send native here (the unwrap payout on a sell). Anything else is a
    // mistake and is refused so funds can't get stuck.
    receive() external payable {
        require(msg.sender == WLXS, "Router: only WLXS");
    }

    function _reserves(address tokenIn, address tokenOut) internal view returns (uint256 rIn, uint256 rOut, address pair) {
        pair = IFactoryr(factory).getPair(tokenIn, tokenOut);
        require(pair != address(0), "Router: no pair");
        (uint112 r0, uint112 r1, ) = IPairr(pair).getReserves();
        (rIn, rOut) = tokenIn < tokenOut ? (uint256(r0), uint256(r1)) : (uint256(r1), uint256(r0));
    }

    // constant-product output with the pair's 0.30% fee.
    function getAmountOut(uint256 amountIn, uint256 rIn, uint256 rOut) public pure returns (uint256) {
        require(amountIn > 0 && rIn > 0 && rOut > 0, "Router: insufficient");
        uint256 inWithFee = amountIn * 997;
        return (inWithFee * rOut) / (rIn * 1000 + inWithFee);
    }

    // read-only quotes for the UI.
    function quoteBuy(address token, uint256 lxsIn) external view returns (uint256) {
        (uint256 rL, uint256 rT, ) = _reserves(WLXS, token);
        return getAmountOut(lxsIn, rL, rT);
    }
    function quoteSell(address token, uint256 tokenIn) external view returns (uint256) {
        (uint256 rT, uint256 rL, ) = _reserves(token, WLXS);
        return getAmountOut(tokenIn, rT, rL);
    }

    // BUY: native LXS -> token, sent to `to`. Wraps, funds the pair, swaps, all atomic.
    function swapExactLXSForTokens(address token, uint256 amountOutMin, address to, uint256 deadline)
        external payable returns (uint256 out)
    {
        require(block.timestamp <= deadline, "Router: expired");
        require(msg.value > 0, "Router: no value");
        (uint256 rL, uint256 rT, address pair) = _reserves(WLXS, token);
        out = getAmountOut(msg.value, rL, rT);
        require(out >= amountOutMin, "Router: slippage");
        IWLXSr(WLXS).deposit{value: msg.value}();
        require(IWLXSr(WLXS).transfer(pair, msg.value), "Router: wlxs seed");
        bool tokenIs0 = token < WLXS;
        IPairr(pair).swap(tokenIs0 ? out : 0, tokenIs0 ? 0 : out, to, new bytes(0));
    }

    // SELL: token -> native LXS, sent to `to`. Caller must approve the router for
    // `amountIn` first. Swaps to WLXS held by the router, unwraps, forwards the LXS.
    function swapExactTokensForLXS(address token, uint256 amountIn, uint256 amountOutMin, address to, uint256 deadline)
        external returns (uint256 out)
    {
        require(block.timestamp <= deadline, "Router: expired");
        require(amountIn > 0, "Router: no amount");
        (uint256 rT, uint256 rL, address pair) = _reserves(token, WLXS);
        out = getAmountOut(amountIn, rT, rL);
        require(out >= amountOutMin, "Router: slippage");
        require(ITokenr(token).transferFrom(msg.sender, pair, amountIn), "Router: token in");
        bool wlxsIs0 = WLXS < token;
        IPairr(pair).swap(wlxsIs0 ? out : 0, wlxsIs0 ? 0 : out, address(this), new bytes(0));
        IWLXSr(WLXS).withdraw(out);
        (bool ok, ) = payable(to).call{value: out}("");
        require(ok, "Router: lxs payout");
    }
}
