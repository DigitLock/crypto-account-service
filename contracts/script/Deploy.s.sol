// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

// Deploys MockUSDC (unless TOKEN is given) and CardSpendController on a local Anvil (chain ID 31337) only.
// Env: DEPLOYER (broadcast sender), TREASURY, ADMIN, OPERATOR; TOKEN optional. No private key: run with
// `forge script --unlocked --sender $DEPLOYER` against Anvil's unlocked accounts. See contracts/README.md.

import {CardSpendController} from "../src/CardSpendController.sol";
import {MockUSDC} from "../src/MockUSDC.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {Script, console} from "forge-std/Script.sol";

contract Deploy is Script {
    uint256 internal constant ANVIL_CHAIN_ID = 31_337;

    /// @notice Reads the addresses from the environment and deploys.
    function run() external returns (address token, address controller) {
        _requireAnvil();
        return deploy(
            vm.envAddress("DEPLOYER"),
            vm.envOr("TOKEN", address(0)),
            vm.envAddress("TREASURY"),
            vm.envAddress("ADMIN"),
            vm.envAddress("OPERATOR")
        );
    }

    /// @notice Deploys MockUSDC when `token` is zero, then the controller; broadcasts as `deployer`.
    function deploy(address deployer, address token, address treasury, address admin, address operator)
        public
        returns (address, address)
    {
        _requireAnvil();
        vm.startBroadcast(deployer);
        if (token == address(0)) token = address(new MockUSDC());
        address controller = address(new CardSpendController(IERC20(token), treasury, admin, operator));
        vm.stopBroadcast();

        console.log("TOKEN=%s", token);
        console.log("CONTROLLER=%s", controller);
        return (token, controller);
    }

    function _requireAnvil() internal view {
        require(block.chainid == ANVIL_CHAIN_ID, "Deploy: chain ID is not 31337 (local Anvil only)");
    }
}
