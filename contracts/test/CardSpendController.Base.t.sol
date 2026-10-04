// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {CardSpendController} from "../src/CardSpendController.sol";
import {MockUSDC} from "../src/MockUSDC.sol";
import {Test} from "forge-std/Test.sol";

abstract contract CardSpendControllerBase is Test {
    // 2026-09-21 14:13:20 UTC; day 20_717 starts at 1_789_948_800.
    uint256 internal constant START = 1_790_000_000;
    // 2027-01-15 08:00:00 UTC.
    uint64 internal constant VALID_UNTIL = 1_800_000_000;

    MockUSDC internal token;
    CardSpendController internal controller;

    address internal admin = makeAddr("admin");
    address internal operator = makeAddr("operator");
    address internal treasury = makeAddr("treasury");
    address internal alice = makeAddr("alice");
    address internal bob = makeAddr("bob");
    address internal stranger = makeAddr("stranger");

    function setUp() public virtual {
        vm.warp(START);
        token = new MockUSDC();
        controller = new CardSpendController(token, treasury, admin, operator);
    }

    function fund(address user, uint256 amount) internal {
        token.mint(user, amount);
        vm.prank(user);
        token.approve(address(controller), amount);
    }

    function limit(address user, uint256 amount) internal {
        vm.prank(admin);
        controller.setDailyLimit(user, amount);
    }

    function debitAs(address caller, address user, uint256 amount, bytes32 authId, uint64 validUntil) internal {
        vm.prank(caller);
        controller.debit(user, amount, authId, validUntil);
    }

    function storedUser(bytes32 authId) internal view returns (address user) {
        (user,,) = controller.authorizations(authId);
    }
}
