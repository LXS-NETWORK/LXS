// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

// UserToken is a standard fixed-supply ERC-20: name, symbol, entire supply minted
// to the deployer at creation. No owner, no mint-after-deploy, no admin. Compiled
// by solc (evmVersion=istanbul) so the same bytecode runs on the LXS VM and reads
// on any Ethereum-compatible explorer or wallet.
contract UserToken {
    string public name;
    string public symbol;
    uint8 public constant decimals = 18;
    uint256 public totalSupply;

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    constructor(string memory _name, string memory _symbol, uint256 _supply) {
        name = _name;
        symbol = _symbol;
        totalSupply = _supply;
        balanceOf[msg.sender] = _supply;
        emit Transfer(address(0), msg.sender, _supply);
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
        require(a >= v, "ERC20: allowance");
        if (a != type(uint256).max) {
            allowance[from][msg.sender] = a - v;
        }
        _transfer(from, to, v);
        return true;
    }

    function _transfer(address from, address to, uint256 v) internal {
        require(balanceOf[from] >= v, "ERC20: balance");
        balanceOf[from] -= v;
        balanceOf[to] += v;
        emit Transfer(from, to, v);
    }
}
