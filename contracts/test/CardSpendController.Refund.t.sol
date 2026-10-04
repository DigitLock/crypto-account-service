// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {CardSpendController} from "../src/CardSpendController.sol";
import {CardSpendControllerBase} from "./CardSpendController.Base.t.sol";
import {IERC20Errors} from "@openzeppelin/contracts/interfaces/draft-IERC6093.sol";

// Phase 4 of docs/test-plan-s1.md.
contract CardSpendControllerRefundTest is CardSpendControllerBase {
    uint256 internal constant LIMIT = 100_000_000; // 100.00 USDC
    uint256 internal constant BALANCE = 1_000_000_000; // 1 000.00 USDC
    uint256 internal constant DEBIT = 50_000_000; // D = 50.00 USDC
    uint256 internal constant PART = 20_000_000; // a = 20.00 USDC

    bytes32 internal constant AUTH_1 = keccak256("auth-1");
    bytes32 internal constant AUTH_2 = keccak256("auth-2");
    bytes32 internal constant REFUND_1 = keccak256("refund-1");
    bytes32 internal constant REFUND_2 = keccak256("refund-2");
    bytes32 internal constant REFUND_3 = keccak256("refund-3");
    bytes32 internal constant REFUND_4 = keccak256("refund-4");

    function setUp() public override {
        super.setUp();
        fund(alice, BALANCE);
        limit(alice, LIMIT);
        fundTreasury(DEBIT);
        debitAs(operator, alice, DEBIT, AUTH_1, VALID_UNTIL);
    }

    function refund(bytes32 authId, bytes32 refundId, uint256 amount) internal {
        refundAs(operator, authId, refundId, amount);
    }

    // S1-T401 — Req: §2.1.5, BR-10, EVM §2.1.2
    function test_T401_partialRefund() public {
        uint256 treasuryBefore = token.balanceOf(treasury);
        uint256 aliceBefore = token.balanceOf(alice);

        vm.expectEmit(true, true, true, true, address(controller));
        emit CardSpendController.Refunded(AUTH_1, REFUND_1, alice, PART);
        refund(AUTH_1, REFUND_1, PART);

        assertEq(token.balanceOf(treasury), treasuryBefore - PART);
        assertEq(token.balanceOf(alice), aliceBefore + PART);
        assertEq(token.balanceOf(address(controller)), 0);

        (uint256 debited, uint256 refunded) = storedAmounts(AUTH_1);
        assertEq(debited, DEBIT);
        assertEq(refunded, PART);
        assertTrue(controller.refundUsed(REFUND_1));
    }

    // S1-T402 — Req: FR-12
    function test_T402_fullRefund() public {
        refund(AUTH_1, REFUND_1, DEBIT);

        (uint256 debited, uint256 refunded) = storedAmounts(AUTH_1);
        assertEq(refunded, debited);
        assertEq(token.balanceOf(alice), BALANCE);
    }

    // S1-T403 — Req: FR-12, BR-10
    function test_T403_partialRefundsUpToDebit() public {
        refund(AUTH_1, REFUND_1, 10_000_000);
        refund(AUTH_1, REFUND_2, 15_000_000);
        refund(AUTH_1, REFUND_3, 25_000_000);

        vm.expectRevert(CardSpendController.RefundExceedsDebit.selector);
        refund(AUTH_1, REFUND_4, 1);

        (, uint256 refunded) = storedAmounts(AUTH_1);
        assertEq(refunded, DEBIT);
        assertEq(token.balanceOf(alice), BALANCE);
        assertFalse(controller.refundUsed(REFUND_4));
    }

    // S1-T404 — Req: §2.1.5
    function test_T404_unknownAuthId() public {
        uint256 treasuryBefore = token.balanceOf(treasury);

        vm.expectRevert(CardSpendController.UnknownAuth.selector);
        refund(keccak256("unknown"), REFUND_1, PART);

        assertEq(token.balanceOf(treasury), treasuryBefore);
        assertEq(token.balanceOf(alice), BALANCE - DEBIT);
        assertFalse(controller.refundUsed(REFUND_1));
    }

    // S1-T405 — Req: §2.1.5, FR-13, BR-10
    function test_T405_refundIdReused() public {
        debitAs(operator, alice, DEBIT, AUTH_2, VALID_UNTIL);
        fundTreasury(DEBIT);
        refund(AUTH_1, REFUND_1, PART);
        uint256 treasuryBefore = token.balanceOf(treasury);
        uint256 aliceBefore = token.balanceOf(alice);

        vm.expectRevert(CardSpendController.RefundAlreadyUsed.selector);
        refund(AUTH_1, REFUND_1, PART);

        vm.expectRevert(CardSpendController.RefundAlreadyUsed.selector);
        refund(AUTH_2, REFUND_1, PART);

        assertEq(token.balanceOf(treasury), treasuryBefore);
        assertEq(token.balanceOf(alice), aliceBefore);
        (, uint256 refunded1) = storedAmounts(AUTH_1);
        (, uint256 refunded2) = storedAmounts(AUTH_2);
        assertEq(refunded1, PART);
        assertEq(refunded2, 0);
    }

    // S1-T406 — Req: §2.1.5, FR-12
    function test_T406_overDebitInOneCall() public {
        vm.expectRevert(CardSpendController.RefundExceedsDebit.selector);
        refund(AUTH_1, REFUND_1, DEBIT + 1);

        (, uint256 refunded) = storedAmounts(AUTH_1);
        assertEq(refunded, 0);
    }

    // S1-T407 — Req: §2.1.5, ADR-9
    function test_T407_refundWhilePaused() public {
        vm.prank(admin);
        controller.pause();

        vm.expectEmit(true, true, true, true, address(controller));
        emit CardSpendController.Refunded(AUTH_1, REFUND_1, alice, PART);
        refund(AUTH_1, REFUND_1, PART);

        assertTrue(controller.paused());
        assertEq(token.balanceOf(alice), BALANCE - DEBIT + PART);
    }

    // S1-T408 — Req: §2.1.5
    function test_T408_zeroRefundAmount() public {
        vm.expectRevert(CardSpendController.ZeroAmount.selector);
        refund(AUTH_1, REFUND_1, 0);

        assertFalse(controller.refundUsed(REFUND_1));
    }

    // S1-T409 — Req: §2.1.5, ADR-9
    function test_T409_recipientFixedByDebit() public {
        // The only refund signature takes no recipient.
        assertEq(controller.refund.selector, bytes4(keccak256("refund(bytes32,bytes32,uint256)")));

        uint256 treasuryBefore = token.balanceOf(treasury);
        refund(AUTH_1, REFUND_1, PART);

        assertEq(token.balanceOf(alice), BALANCE - DEBIT + PART);
        assertEq(token.balanceOf(operator), 0);
        assertEq(token.balanceOf(treasury), treasuryBefore - PART);
        assertEq(token.balanceOf(address(controller)), 0);
    }

    // S1-T410 — Req: §2.1.5, EC-12
    function test_T410_treasuryCannotFund() public {
        uint256 allowance = PART - 1;
        vm.prank(treasury);
        token.approve(address(controller), allowance);

        vm.expectRevert(
            abi.encodeWithSelector(
                IERC20Errors.ERC20InsufficientAllowance.selector, address(controller), allowance, PART
            )
        );
        refund(AUTH_1, REFUND_1, PART);

        assertFalse(controller.refundUsed(REFUND_1));
        (, uint256 refunded) = storedAmounts(AUTH_1);
        assertEq(refunded, 0);

        uint256 balance = PART - 1;
        vm.startPrank(treasury);
        token.approve(address(controller), PART);
        token.transfer(stranger, token.balanceOf(treasury) - balance);
        vm.stopPrank();

        vm.expectRevert(abi.encodeWithSelector(IERC20Errors.ERC20InsufficientBalance.selector, treasury, balance, PART));
        refund(AUTH_1, REFUND_1, PART);

        assertFalse(controller.refundUsed(REFUND_1));
        (, refunded) = storedAmounts(AUTH_1);
        assertEq(refunded, 0);
        assertEq(token.balanceOf(alice), BALANCE - DEBIT);
    }

    // S1-T411 — Req: §2.1.5
    function test_T411_checkOrder() public {
        uint256 earlier = 10_000_000;
        refund(AUTH_1, REFUND_1, earlier);

        // Every rule fails at once: zero amount, unknown authId, used refundId.
        bytes32 authId = keccak256("unknown");
        bytes32 refundId = REFUND_1;
        uint256 amount = 0;

        vm.expectRevert(CardSpendController.ZeroAmount.selector);
        refund(authId, refundId, amount);

        amount = DEBIT - earlier + 1; // over what is left of the debit
        vm.expectRevert(CardSpendController.UnknownAuth.selector);
        refund(authId, refundId, amount);

        authId = AUTH_1;
        vm.expectRevert(CardSpendController.RefundAlreadyUsed.selector);
        refund(authId, refundId, amount);

        refundId = REFUND_2;
        vm.expectRevert(CardSpendController.RefundExceedsDebit.selector);
        refund(authId, refundId, amount);

        amount = DEBIT - earlier;
        refund(authId, refundId, amount);

        (, uint256 refunded) = storedAmounts(AUTH_1);
        assertEq(refunded, DEBIT);
        assertEq(token.balanceOf(alice), BALANCE);
    }
}
