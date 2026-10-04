// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {CardSpendController} from "../src/CardSpendController.sol";
import {CardSpendControllerBase} from "./CardSpendController.Base.t.sol";
import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";
import {IERC20Errors} from "@openzeppelin/contracts/interfaces/draft-IERC6093.sol";
import {Pausable} from "@openzeppelin/contracts/utils/Pausable.sol";

// Phase 3 of docs/test-plan-s1.md.
contract CardSpendControllerDebitTest is CardSpendControllerBase {
    uint256 internal constant LIMIT = 100_000_000; // 100.00 USDC
    uint256 internal constant BALANCE = 1_000_000_000; // 1 000.00 USDC
    uint256 internal constant AMOUNT = 25_400_000; // 25.40 USDC

    bytes32 internal constant AUTH_1 = keccak256("auth-1");
    bytes32 internal constant AUTH_2 = keccak256("auth-2");
    bytes32 internal constant AUTH_3 = keccak256("auth-3");
    bytes32 internal constant AUTH_4 = keccak256("auth-4");

    function setUp() public override {
        super.setUp();
        fund(alice, BALANCE);
        limit(alice, LIMIT);
    }

    function debit(address user, uint256 amount, bytes32 authId) internal {
        debitAs(operator, user, amount, authId, VALID_UNTIL);
    }

    function assertUntouched(address user, uint256 userBalance) internal view {
        assertEq(token.balanceOf(user), userBalance);
        assertEq(token.balanceOf(treasury), 0);
        assertEq(token.balanceOf(address(controller)), 0);
    }

    // S1-T301 — Req: §2.1.5, BR-6, EVM §2.1.2
    function test_T301_happyPath() public {
        assertEq(controller.remainingDailyLimit(alice), LIMIT);

        vm.expectEmit(true, true, false, true, address(controller));
        emit CardSpendController.Debited(AUTH_1, alice, AMOUNT);
        debit(alice, AMOUNT, AUTH_1);

        assertEq(token.balanceOf(alice), BALANCE - AMOUNT);
        assertEq(token.balanceOf(treasury), AMOUNT);
        assertEq(token.balanceOf(address(controller)), 0);

        (address user, uint256 debited, uint256 refunded) = controller.authorizations(AUTH_1);
        assertEq(user, alice);
        assertEq(debited, AMOUNT);
        assertEq(refunded, 0);

        assertEq(controller.remainingDailyLimit(alice), LIMIT - AMOUNT);
    }

    // S1-T302 — Req: §2.1.5, BR-8
    function test_T302_authIdReused() public {
        fund(bob, BALANCE);
        limit(bob, LIMIT);
        debit(alice, AMOUNT, AUTH_1);

        vm.expectRevert(CardSpendController.AuthAlreadyUsed.selector);
        debit(alice, AMOUNT, AUTH_1);

        vm.expectRevert(CardSpendController.AuthAlreadyUsed.selector);
        debit(bob, 1_000_000, AUTH_1);

        assertEq(token.balanceOf(alice), BALANCE - AMOUNT);
        assertEq(token.balanceOf(bob), BALANCE);
        assertEq(token.balanceOf(treasury), AMOUNT);
    }

    // S1-T303 — Req: §2.1.5, FR-6, ADR-12
    function test_T303_expired() public {
        vm.warp(uint256(VALID_UNTIL) + 1);

        vm.expectRevert(CardSpendController.AuthExpired.selector);
        debit(alice, AMOUNT, AUTH_1);

        assertUntouched(alice, BALANCE);
        assertEq(storedUser(AUTH_1), address(0));
    }

    // S1-T304 — Req: §2.1.5
    function test_T304_boundaryAtValidUntil() public {
        vm.warp(VALID_UNTIL);

        debit(alice, AMOUNT, AUTH_1);

        assertEq(token.balanceOf(treasury), AMOUNT);
        assertEq(storedUser(AUTH_1), alice);
    }

    // S1-T305 — Req: §2.1.5
    function test_T305_zeroAmount() public {
        vm.expectRevert(CardSpendController.ZeroAmount.selector);
        debit(alice, 0, AUTH_1);

        assertUntouched(alice, BALANCE);
    }

    // S1-T306 — Req: §2.1.5, BR-9
    function test_T306_overDailyLimit() public {
        uint256 spent = 30_000_000;
        debit(alice, spent, AUTH_1);

        vm.expectRevert(CardSpendController.DailyLimitExceeded.selector);
        debit(alice, LIMIT - spent + 1, AUTH_2);

        debit(alice, LIMIT - spent, AUTH_3);

        assertEq(token.balanceOf(treasury), LIMIT);
        assertEq(controller.remainingDailyLimit(alice), 0);
    }

    // S1-T307 — Req: §2.1.5
    function test_T307_defaultLimitZero() public {
        fund(bob, BALANCE);

        vm.expectRevert(CardSpendController.DailyLimitExceeded.selector);
        debit(bob, 1, AUTH_1);

        assertUntouched(bob, BALANCE);
    }

    // S1-T308 — Req: §2.1.5, BR-9
    function test_T308_paused() public {
        vm.prank(admin);
        controller.pause();

        vm.expectRevert(Pausable.EnforcedPause.selector);
        debit(alice, AMOUNT, AUTH_1);

        assertUntouched(alice, BALANCE);
    }

    // S1-T309 — Req: §2.1.5
    function test_T309_allowanceTooLow() public {
        uint256 allowance = AMOUNT - 1;
        token.mint(bob, BALANCE);
        vm.prank(bob);
        token.approve(address(controller), allowance);
        limit(bob, LIMIT);

        vm.expectRevert(
            abi.encodeWithSelector(
                IERC20Errors.ERC20InsufficientAllowance.selector, address(controller), allowance, AMOUNT
            )
        );
        debit(bob, AMOUNT, AUTH_1);

        assertEq(storedUser(AUTH_1), address(0));
        assertUntouched(bob, BALANCE);
    }

    // S1-T310 — Req: §2.1.5
    function test_T310_balanceTooLow() public {
        uint256 balance = AMOUNT - 1;
        token.mint(bob, balance);
        vm.prank(bob);
        token.approve(address(controller), AMOUNT);
        limit(bob, LIMIT);

        vm.expectRevert(abi.encodeWithSelector(IERC20Errors.ERC20InsufficientBalance.selector, bob, balance, AMOUNT));
        debit(bob, AMOUNT, AUTH_1);

        assertEq(storedUser(AUTH_1), address(0));
        assertUntouched(bob, balance);
    }

    // S1-T311 — Req: §2.1.5, BR-9
    function test_T311_utcDayRollover() public {
        uint256 day = vm.getBlockTimestamp() / 1 days;
        uint256 lastSecondOfDay = day * 1 days + 1 days - 1; // D 23:59:59 UTC
        uint256 startOfNextDay = (day + 1) * 1 days; // (D+1) 00:00:00 UTC
        assertEq(lastSecondOfDay, 1_790_035_199);
        assertEq(startOfNextDay, 1_790_035_200);

        debit(alice, LIMIT, AUTH_1);

        vm.warp(lastSecondOfDay);
        assertEq(controller.remainingDailyLimit(alice), 0);
        vm.expectRevert(CardSpendController.DailyLimitExceeded.selector);
        debit(alice, 1, AUTH_2);

        vm.warp(startOfNextDay);
        assertEq(controller.remainingDailyLimit(alice), LIMIT);
        debit(alice, LIMIT, AUTH_3);

        assertEq(token.balanceOf(treasury), 2 * LIMIT);
        assertEq(controller.remainingDailyLimit(alice), 0);
    }

    // S1-T312 — Req: §2.1.5, ADR-9
    function test_T312_refundDoesNotRestoreLimit() public {
        // enabled in st5: needs refund.
        vm.skip(true);
    }

    // S1-T313 — Req: BR-9
    function test_T313_limitsArePerWallet() public {
        fund(bob, BALANCE);
        limit(bob, LIMIT);

        debit(alice, LIMIT, AUTH_1);

        assertEq(controller.remainingDailyLimit(alice), 0);
        assertEq(controller.remainingDailyLimit(bob), LIMIT);

        debit(bob, AMOUNT, AUTH_2);

        assertEq(token.balanceOf(bob), BALANCE - AMOUNT);
        assertEq(controller.remainingDailyLimit(bob), LIMIT - AMOUNT);
    }

    // S1-T314 — Req: §2.1.5
    function test_T314_zeroUser() public {
        vm.expectRevert(CardSpendController.ZeroAddress.selector);
        debit(address(0), AMOUNT, AUTH_1);

        assertEq(storedUser(AUTH_1), address(0));
        assertUntouched(alice, BALANCE);
    }

    // S1-T315 — Req: §2.1.5
    function test_T315_zeroAuthId() public {
        debit(alice, AMOUNT, bytes32(0));

        assertEq(storedUser(bytes32(0)), alice);
        assertEq(token.balanceOf(treasury), AMOUNT);

        vm.expectRevert(CardSpendController.AuthAlreadyUsed.selector);
        debit(alice, AMOUNT, bytes32(0));
    }

    // S1-T316 — Req: §2.1.5
    function test_T316_severalDebitsSameDay() public {
        bytes32[3] memory ids = [AUTH_1, AUTH_2, AUTH_3];
        uint256[3] memory amounts = [uint256(25_000_000), 25_000_000, 50_000_000];

        for (uint256 i; i < ids.length; ++i) {
            debit(alice, amounts[i], ids[i]);
        }

        vm.expectRevert(CardSpendController.DailyLimitExceeded.selector);
        debit(alice, 1, AUTH_4);

        for (uint256 i; i < ids.length; ++i) {
            (address user, uint256 debited, uint256 refunded) = controller.authorizations(ids[i]);
            assertEq(user, alice);
            assertEq(debited, amounts[i]);
            assertEq(refunded, 0);
        }
        assertEq(storedUser(AUTH_4), address(0));
        assertEq(token.balanceOf(treasury), LIMIT);
        assertEq(controller.remainingDailyLimit(alice), 0);
    }

    // S1-T317 — Req: §2.1.5
    function test_T317_checkOrder() public {
        uint256 spent = 10_000_000;
        debit(alice, spent, AUTH_1);
        vm.prank(admin);
        controller.pause();

        // Every rule fails at once: wrong caller, paused, zero amount, zero user, expired, used authId.
        address user = address(0);
        uint256 amount = 0;
        bytes32 authId = AUTH_1;
        uint64 validUntil = uint64(block.timestamp - 1);

        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector, stranger, controller.OPERATOR_ROLE()
            )
        );
        debitAs(stranger, user, amount, authId, validUntil);

        vm.expectRevert(Pausable.EnforcedPause.selector);
        debitAs(operator, user, amount, authId, validUntil);

        vm.prank(admin);
        controller.unpause();
        vm.expectRevert(CardSpendController.ZeroAmount.selector);
        debitAs(operator, user, amount, authId, validUntil);

        amount = LIMIT - spent + 1; // over today's limit
        vm.expectRevert(CardSpendController.ZeroAddress.selector);
        debitAs(operator, user, amount, authId, validUntil);

        user = alice;
        vm.expectRevert(CardSpendController.AuthExpired.selector);
        debitAs(operator, user, amount, authId, validUntil);

        validUntil = VALID_UNTIL;
        vm.expectRevert(CardSpendController.AuthAlreadyUsed.selector);
        debitAs(operator, user, amount, authId, validUntil);

        authId = AUTH_2;
        vm.expectRevert(CardSpendController.DailyLimitExceeded.selector);
        debitAs(operator, user, amount, authId, validUntil);

        amount = LIMIT - spent;
        debitAs(operator, user, amount, authId, validUntil);
        assertEq(token.balanceOf(treasury), LIMIT);
    }
}
