// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {CardSpendController} from "../src/CardSpendController.sol";
import {CardSpendControllerBase} from "./CardSpendController.Base.t.sol";
import {Pausable} from "@openzeppelin/contracts/utils/Pausable.sol";

// Phase 5 of docs/test-plan-s1.md.
contract CardSpendControllerAdminTest is CardSpendControllerBase {
    uint256 internal constant LIMIT = 100_000_000; // 100.00 USDC
    uint256 internal constant BALANCE = 1_000_000_000; // 1 000.00 USDC

    bytes32 internal constant AUTH_1 = keccak256("auth-1");
    bytes32 internal constant AUTH_2 = keccak256("auth-2");

    function setUp() public override {
        super.setUp();
        fund(alice, BALANCE);
    }

    function debit(uint256 amount, bytes32 authId) internal {
        debitAs(operator, alice, amount, authId, VALID_UNTIL);
    }

    // S1-T501 — Req: §2.1.5, BR-9
    function test_T501_setLimit() public {
        vm.expectEmit(true, false, false, true, address(controller));
        emit CardSpendController.DailyLimitSet(alice, LIMIT);
        vm.prank(admin);
        controller.setDailyLimit(alice, LIMIT);

        assertEq(controller.remainingDailyLimit(alice), LIMIT);
    }

    // S1-T502 — Req: §2.1.5
    function test_T502_lowerLimitBelowTodaysSpend() public {
        uint256 spent = 60_000_000;
        limit(alice, LIMIT);
        debit(spent, AUTH_1);

        limit(alice, 40_000_000);
        assertEq(controller.remainingDailyLimit(alice), 0);

        vm.expectRevert(CardSpendController.DailyLimitExceeded.selector);
        debit(1, AUTH_2);

        assertEq(token.balanceOf(alice), BALANCE - spent);
        assertEq(token.balanceOf(treasury), spent);
    }

    // S1-T503 — Req: §2.1.5
    function test_T503_raiseLimitMidDay() public {
        uint256 spent = 30_000_000;
        uint256 raised = 150_000_000;
        limit(alice, LIMIT);
        debit(spent, AUTH_1);

        limit(alice, raised);
        assertEq(controller.remainingDailyLimit(alice), raised - spent);

        debit(raised - spent, AUTH_2);
        assertEq(controller.remainingDailyLimit(alice), 0);
        assertEq(token.balanceOf(treasury), raised);
    }

    // S1-T504 — Req: BR-9
    function test_T504_limitZeroStopsSpending() public {
        limit(alice, LIMIT);
        limit(alice, 0);

        vm.expectRevert(CardSpendController.DailyLimitExceeded.selector);
        debit(1, AUTH_1);

        assertEq(token.balanceOf(alice), BALANCE);
    }

    // S1-T505 — Req: §2.1.5
    function test_T505_pauseAndUnpause() public {
        vm.expectEmit(address(controller));
        emit Pausable.Paused(admin);
        vm.prank(admin);
        controller.pause();
        assertTrue(controller.paused());

        vm.expectEmit(address(controller));
        emit Pausable.Unpaused(admin);
        vm.prank(admin);
        controller.unpause();
        assertFalse(controller.paused());
    }

    // S1-T506 — Req: §2.1.5
    function test_T506_doublePauseUnpause() public {
        vm.expectRevert(Pausable.ExpectedPause.selector);
        vm.prank(admin);
        controller.unpause();

        vm.prank(admin);
        controller.pause();

        vm.expectRevert(Pausable.EnforcedPause.selector);
        vm.prank(admin);
        controller.pause();

        vm.prank(admin);
        controller.unpause();

        vm.expectRevert(Pausable.ExpectedPause.selector);
        vm.prank(admin);
        controller.unpause();
    }

    // S1-T507 — Req: BR-9
    function test_T507_debitAfterUnpause() public {
        limit(alice, LIMIT);
        vm.startPrank(admin);
        controller.pause();
        controller.unpause();
        vm.stopPrank();

        debit(LIMIT, AUTH_1);

        assertEq(token.balanceOf(treasury), LIMIT);
        assertEq(storedUser(AUTH_1), alice);
    }
}
