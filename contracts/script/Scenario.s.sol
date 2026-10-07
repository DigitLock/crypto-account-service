// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

// Full cycle of docs/test-plan-s1.md row S1-T702 on a deployed pair, on a test network only: local Anvil
// (chain ID 31337) or Base Sepolia (chain ID 84532); any other chain ID reverts.
// mint → approve → setDailyLimit → debit → partial refund, then checks every balance and view.
// Env: TOKEN, CONTROLLER, ADMIN, OPERATOR, TREASURY, USER. Expects a fresh pair: USER and TREASURY start with 0 USDC.
// On Anvil no private key: run with `forge script --unlocked --sender $USER`; each step broadcasts as its actor.

import {CardSpendController} from "../src/CardSpendController.sol";
import {MockUSDC} from "../src/MockUSDC.sol";
import {Script, console} from "forge-std/Script.sol";

contract Scenario is Script {
    uint256 internal constant ANVIL_CHAIN_ID = 31_337;
    uint256 internal constant BASE_SEPOLIA_CHAIN_ID = 84_532;

    uint256 internal constant MINT = 100_000_000; // 100.00 USDC
    uint256 internal constant DAILY_LIMIT = 50_000_000; // 50.00 USDC
    uint256 internal constant DEBIT = 25_400_000; // 25.40 USDC
    uint256 internal constant REFUND = 10_000_000; // 10.00 USDC
    bytes32 internal constant AUTH_ID = keccak256("scenario-auth-1");
    bytes32 internal constant REFUND_ID = keccak256("scenario-refund-1");

    /// @notice Runs the cycle and reverts if any expected value does not hold.
    function run() external {
        require(
            block.chainid == ANVIL_CHAIN_ID || block.chainid == BASE_SEPOLIA_CHAIN_ID,
            "Scenario: chain ID is not 31337 (Anvil) or 84532 (Base Sepolia)"
        );

        MockUSDC token = MockUSDC(vm.envAddress("TOKEN"));
        CardSpendController controller = CardSpendController(vm.envAddress("CONTROLLER"));
        address admin = vm.envAddress("ADMIN");
        address operator = vm.envAddress("OPERATOR");
        address treasury = vm.envAddress("TREASURY");
        address user = vm.envAddress("USER");
        require(address(controller.token()) == address(token), "Scenario: TOKEN is not the controller's token");
        require(controller.treasury() == treasury, "Scenario: TREASURY is not the controller's treasury");
        uint64 validUntil = uint64(block.timestamp + 3600);

        vm.startBroadcast(user);
        token.mint(user, MINT);
        token.approve(address(controller), MINT);
        vm.stopBroadcast();

        vm.startBroadcast(admin);
        controller.setDailyLimit(user, DAILY_LIMIT);
        vm.stopBroadcast();

        vm.startBroadcast(operator);
        controller.debit(user, DEBIT, AUTH_ID, validUntil);
        vm.stopBroadcast();

        vm.startBroadcast(treasury);
        token.approve(address(controller), REFUND);
        vm.stopBroadcast();

        vm.startBroadcast(operator);
        controller.refund(AUTH_ID, REFUND_ID, REFUND);
        vm.stopBroadcast();

        uint256 userBalance = token.balanceOf(user);
        uint256 treasuryBalance = token.balanceOf(treasury);
        uint256 controllerBalance = token.balanceOf(address(controller));
        (address authUser, uint256 debited, uint256 refunded) = controller.authorizations(AUTH_ID);
        uint256 remaining = controller.remainingDailyLimit(user);
        bool refundUsed = controller.refundUsed(REFUND_ID);

        console.log("USER balance (base units):       ", userBalance);
        console.log("TREASURY balance (base units):   ", treasuryBalance);
        console.log("CONTROLLER balance (base units): ", controllerBalance);
        console.log("authorizations(authId).user:     ", authUser);
        console.log("authorizations(authId).debited:  ", debited);
        console.log("authorizations(authId).refunded: ", refunded);
        console.log("remainingDailyLimit(USER):       ", remaining);
        console.log("refundUsed(refundId):            ", refundUsed);

        require(userBalance == MINT - DEBIT + REFUND, "Scenario: USER balance is not 84.60 USDC");
        require(treasuryBalance == DEBIT - REFUND, "Scenario: TREASURY balance is not 15.40 USDC");
        require(controllerBalance == 0, "Scenario: CONTROLLER holds tokens");
        require(authUser == user, "Scenario: authorization user is not USER");
        require(debited == DEBIT, "Scenario: debited is not 25.40 USDC");
        require(refunded == REFUND, "Scenario: refunded is not 10.00 USDC");
        require(remaining == DAILY_LIMIT - DEBIT, "Scenario: remainingDailyLimit is not 24.60 USDC");
        require(refundUsed, "Scenario: refundId is not marked used");
    }
}
