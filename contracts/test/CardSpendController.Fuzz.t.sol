// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {CardSpendController} from "../src/CardSpendController.sol";
import {CardSpendControllerBase} from "./CardSpendController.Base.t.sol";

// Phase 6 of docs/test-plan-s1.md, stateless fuzz tests T607 and T608.
contract CardSpendControllerFuzzTest is CardSpendControllerBase {
    uint256 internal constant LIMIT = 100_000_000; // 100.00 USDC
    uint256 internal constant AMOUNT = 25_400_000; // 25.40 USDC
    bytes32 internal constant AUTH_1 = keccak256("auth-1");

    // S1-T607 — Req: FR-6
    function testFuzz_T607_expiry(uint64 validUntil, uint64 warpTo) public {
        validUntil = uint64(bound(validUntil, START, START + 30 days));
        warpTo = uint64(bound(warpTo, START, START + 30 days));
        fund(alice, AMOUNT);
        limit(alice, LIMIT);

        vm.warp(warpTo);
        if (warpTo <= validUntil) {
            debitAs(operator, alice, AMOUNT, AUTH_1, validUntil);
            assertEq(token.balanceOf(treasury), AMOUNT);
            assertEq(storedUser(AUTH_1), alice);
        } else {
            vm.expectRevert(CardSpendController.AuthExpired.selector);
            debitAs(operator, alice, AMOUNT, AUTH_1, validUntil);
            assertEq(storedUser(AUTH_1), address(0));
        }
    }

    // S1-T608 — Req: §2.1.5
    function testFuzz_T608_dayArithmetic(uint256 ts, uint256 dailyLimit, uint256 amount) public {
        ts = bound(ts, START, START + 3650 days);
        dailyLimit = bound(dailyLimit, 1, 1_000_000_000e6);
        amount = bound(amount, 1, dailyLimit);
        uint256 nextDay = (ts / 1 days + 1) * 1 days;

        vm.warp(ts);
        fund(alice, amount);
        limit(alice, dailyLimit);
        debitAs(operator, alice, amount, AUTH_1, uint64(ts));
        assertEq(controller.remainingDailyLimit(alice), dailyLimit - amount);

        vm.warp(nextDay - 1);
        assertEq(controller.remainingDailyLimit(alice), dailyLimit - amount);

        vm.warp(nextDay);
        assertEq(controller.remainingDailyLimit(alice), dailyLimit);
    }
}
