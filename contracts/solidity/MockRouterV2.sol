// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

interface IERC20 {
    function transferFrom(address from, address to, uint256 value) external returns (bool);
    function balanceOf(address who) external view returns (uint256);
}

// A stand-in for UniswapV2Router02.addLiquidity used ONLY in local tests. It pulls both
// tokens from the caller exactly as the real router does (transferFrom after the caller
// approves), so the graduation orchestrator's approve + addLiquidity call path is
// exercised end to end without a real DEX. In production the real router on Base is used.
contract MockRouterV2 {
    event LiquidityAdded(address indexed tokenA, address indexed tokenB, uint256 amountA, uint256 amountB, address to);

    function addLiquidity(
        address tokenA,
        address tokenB,
        uint256 amountADesired,
        uint256 amountBDesired,
        uint256, /*amountAMin*/
        uint256, /*amountBMin*/
        address to,
        uint256 /*deadline*/
    ) external returns (uint256 amountA, uint256 amountB, uint256 liquidity) {
        require(IERC20(tokenA).transferFrom(msg.sender, address(this), amountADesired), "pull A");
        require(IERC20(tokenB).transferFrom(msg.sender, address(this), amountBDesired), "pull B");
        emit LiquidityAdded(tokenA, tokenB, amountADesired, amountBDesired, to);
        return (amountADesired, amountBDesired, amountADesired);
    }
}
