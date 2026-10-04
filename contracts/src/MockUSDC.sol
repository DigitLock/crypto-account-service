// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

// Test funding token of CAS (ADR-8). Anyone can mint. Never a mainnet token.

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {ERC20Burnable} from "@openzeppelin/contracts/token/ERC20/extensions/ERC20Burnable.sol";

contract MockUSDC is ERC20, ERC20Burnable {
    constructor() ERC20("Mock USDC", "USDC") {}

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }
}
