// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {CardSpendController} from "../src/CardSpendController.sol";
import {CardSpendControllerBase} from "./CardSpendController.Base.t.sol";
import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";

// Phase 2 of docs/test-plan-s1.md. T205 and T212 need refund and are added in st5.
contract CardSpendControllerSetupTest is CardSpendControllerBase {
    bytes32 internal constant ADMIN_ROLE = 0x00;
    bytes32 internal constant OPERATOR_ROLE = keccak256("OPERATOR_ROLE");

    function expectUnauthorized(address account, bytes32 role) internal {
        vm.expectRevert(abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, account, role));
    }

    // S1-T201 — Req: §2.1.5
    function test_T201_immutables() public view {
        assertEq(address(controller.token()), address(token));
        assertEq(controller.treasury(), treasury);
    }

    // S1-T202 — Req: §2.1.5, ADR-9
    function test_T202_rolesAtDeployment() public view {
        assertEq(controller.DEFAULT_ADMIN_ROLE(), ADMIN_ROLE);
        assertEq(controller.OPERATOR_ROLE(), OPERATOR_ROLE);
        assertEq(controller.getRoleAdmin(OPERATOR_ROLE), ADMIN_ROLE);

        assertTrue(controller.hasRole(ADMIN_ROLE, admin));
        assertTrue(controller.hasRole(OPERATOR_ROLE, operator));

        assertFalse(controller.hasRole(OPERATOR_ROLE, admin));
        assertFalse(controller.hasRole(ADMIN_ROLE, operator));
        address[4] memory others = [treasury, alice, stranger, address(this)];
        for (uint256 i; i < others.length; ++i) {
            assertFalse(controller.hasRole(ADMIN_ROLE, others[i]));
            assertFalse(controller.hasRole(OPERATOR_ROLE, others[i]));
        }
    }

    // S1-T203 — Req: §2.1.5
    function test_T203_zeroConstructorArgument() public {
        vm.expectRevert(CardSpendController.ZeroAddress.selector);
        new CardSpendController(IERC20(address(0)), treasury, admin, operator);

        vm.expectRevert(CardSpendController.ZeroAddress.selector);
        new CardSpendController(token, address(0), admin, operator);

        vm.expectRevert(CardSpendController.ZeroAddress.selector);
        new CardSpendController(token, treasury, address(0), operator);

        vm.expectRevert(CardSpendController.ZeroAddress.selector);
        new CardSpendController(token, treasury, admin, address(0));
    }

    // S1-T204 — Req: ADR-9
    function test_T204_debitByNonOperator() public {
        fund(alice, 10_000_000);
        limit(alice, 10_000_000);

        expectUnauthorized(admin, OPERATOR_ROLE);
        debitAs(admin, alice, 1_000_000, keccak256("auth-1"), VALID_UNTIL);

        expectUnauthorized(stranger, OPERATOR_ROLE);
        debitAs(stranger, alice, 1_000_000, keccak256("auth-1"), VALID_UNTIL);

        assertEq(token.balanceOf(alice), 10_000_000);
    }

    // S1-T206 — Req: ADR-9, BR-9
    function test_T206_setDailyLimitByNonAdmin() public {
        expectUnauthorized(operator, ADMIN_ROLE);
        vm.prank(operator);
        controller.setDailyLimit(alice, 1_000_000);

        expectUnauthorized(stranger, ADMIN_ROLE);
        vm.prank(stranger);
        controller.setDailyLimit(alice, 1_000_000);

        assertEq(controller.remainingDailyLimit(alice), 0);
    }

    // S1-T207 — Req: ADR-9, BR-9
    function test_T207_pauseUnpauseByNonAdmin() public {
        expectUnauthorized(operator, ADMIN_ROLE);
        vm.prank(operator);
        controller.pause();

        expectUnauthorized(stranger, ADMIN_ROLE);
        vm.prank(stranger);
        controller.pause();

        assertFalse(controller.paused());

        vm.prank(admin);
        controller.pause();

        expectUnauthorized(operator, ADMIN_ROLE);
        vm.prank(operator);
        controller.unpause();

        expectUnauthorized(stranger, ADMIN_ROLE);
        vm.prank(stranger);
        controller.unpause();

        assertTrue(controller.paused());
    }

    // S1-T208 — Req: §2.1.5, ADR-9
    function test_T208_adminRotatesOperator() public {
        address newOperator = makeAddr("newOperator");
        fund(alice, 10_000_000);
        limit(alice, 10_000_000);

        vm.startPrank(admin);
        controller.grantRole(OPERATOR_ROLE, newOperator);
        controller.revokeRole(OPERATOR_ROLE, operator);
        vm.stopPrank();

        debitAs(newOperator, alice, 1_000_000, keccak256("auth-1"), VALID_UNTIL);
        assertEq(token.balanceOf(treasury), 1_000_000);

        expectUnauthorized(operator, OPERATOR_ROLE);
        debitAs(operator, alice, 1_000_000, keccak256("auth-2"), VALID_UNTIL);
    }

    // S1-T209 — Req: G-3, BR-6
    function test_T209_controllerHoldsNothingAtStart() public view {
        assertEq(token.balanceOf(address(controller)), 0);
    }

    // S1-T210 — Req: §2.1.5
    function test_T210_unknownAuthIdView() public view {
        (address user, uint256 debited, uint256 refunded) = controller.authorizations(keccak256("unknown"));
        assertEq(user, address(0));
        assertEq(debited, 0);
        assertEq(refunded, 0);
    }

    // S1-T211 — Req: §2.1.5
    function test_T211_remainingDailyLimitWithoutLimit() public view {
        assertEq(controller.remainingDailyLimit(alice), 0);
    }
}
