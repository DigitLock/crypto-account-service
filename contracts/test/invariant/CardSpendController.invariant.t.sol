// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {CardSpendControllerBase} from "../CardSpendController.Base.t.sol";
import {Handler} from "./Handler.sol";
import {console} from "forge-std/console.sol";

// Phase 6 of docs/test-plan-s1.md, invariants T601 … T606.
contract CardSpendControllerInvariantTest is CardSpendControllerBase {
    // Handler calls after which the campaign must have made real debits and refunds. Below the smallest
    // runs × depth of foundry.toml: 256 × 15 = 3 840 (default), 500 × 50 = 25 000 (ci).
    uint256 internal constant MIN_CALLS = 3000;

    // EVM state, the handler's counters included, resets between runs; the process environment does not.
    // The cumulative counters of the campaign therefore live in environment variables.
    string internal constant ENV_CALLS = "CAS_S1_INVARIANT_CALLS";
    string internal constant ENV_DEBITS = "CAS_S1_INVARIANT_DEBITS";
    string internal constant ENV_REFUNDS = "CAS_S1_INVARIANT_REFUNDS";
    string internal constant ENV_REVERTS = "CAS_S1_INVARIANT_REVERTS";

    Handler internal handler;

    function setUp() public override {
        super.setUp();
        address[4] memory users = [alice, bob, makeAddr("carol"), makeAddr("dave")];
        handler = new Handler(controller, token, admin, operator, treasury, users);

        targetContract(address(handler));
        excludeContract(address(controller));
        excludeContract(address(token));
    }

    // S1-T601 — Req: G-3, BR-6
    function invariant_T601_controllerHoldsNothing() public view {
        assertEq(token.balanceOf(address(controller)), 0);
    }

    // S1-T602 — Req: FR-12, BR-10
    function invariant_T602_refundedNeverAboveDebited() public view {
        uint256 n = handler.authIdCount();
        for (uint256 i; i < n; ++i) {
            bytes32 authId = handler.authIds(i);
            (uint256 debited, uint256 refunded) = storedAmounts(authId);
            assertLe(refunded, debited);
            assertEq(refunded, handler.ghostRefunded(authId));
        }
        assertEq(handler.refundMismatches(), 0, "refund accepted over the debit or rejected within it");
    }

    // S1-T603 — Req: BR-8
    function invariant_T603_authIdSingleUse() public view {
        uint256 n = handler.authIdCount();
        for (uint256 i; i < n; ++i) {
            bytes32 authId = handler.authIds(i);
            (uint256 debited,) = storedAmounts(authId);
            assertEq(storedUser(authId), handler.ghostUser(authId));
            assertEq(debited, handler.ghostDebited(authId));
        }
        assertEq(handler.debitReplaysAccepted(), 0, "debit with a used authId did not revert AuthAlreadyUsed");
    }

    // S1-T604 — Req: FR-13, BR-10
    function invariant_T604_refundIdSingleUse() public view {
        uint256 n = handler.refundIdCount();
        for (uint256 i; i < n; ++i) {
            assertTrue(controller.refundUsed(handler.refundIds(i)));
        }
        n = handler.replayedRefundIdCount();
        for (uint256 i; i < n; ++i) {
            assertTrue(controller.refundUsed(handler.replayedRefundIds(i)));
        }
        assertEq(handler.refundReplaysAccepted(), 0, "refund with a used refundId did not revert RefundAlreadyUsed");
    }

    // S1-T605 — Req: BR-6
    function invariant_T605_conservation() public view {
        assertEq(token.balanceOf(treasury), handler.treasuryInitial() + handler.sumDebited() - handler.sumRefunded());
    }

    // S1-T606 — Req: BR-9
    function invariant_T606_dailySpendWithinLimit() public view {
        uint256 n = handler.dayRecordCount();
        for (uint256 i; i < n; ++i) {
            (address user, uint256 day) = handler.dayRecordAt(i);
            assertLe(handler.daySpend(user, day), handler.limitAtLastDebit(user, day));
        }
        for (uint256 i; i < 4; ++i) {
            address user = handler.userAt(i);
            assertEq(controller.remainingDailyLimit(user), handler.ghostRemainingToday(user));
        }
        assertEq(handler.debitMismatches(), 0, "debit outcome differs from the rules of 2.1.5");
    }

    // The suite must not pass trivially: once the campaign has made MIN_CALLS handler calls,
    // it must have made at least one successful debit and one successful refund.
    function afterInvariant() public {
        uint256 calls = _accumulate(ENV_CALLS, handler.calls());
        uint256 debits = _accumulate(ENV_DEBITS, handler.debits());
        uint256 refunds = _accumulate(ENV_REFUNDS, handler.refunds());
        uint256 reverts = _accumulate(ENV_REVERTS, handler.revertsCaught());

        console.log("run:        calls %d, debits %d, refunds %d", handler.calls(), handler.debits(), handler.refunds());
        console.log("run:        reverts caught %d", handler.revertsCaught());
        console.log("cumulative: calls %d, debits %d, refunds %d", calls, debits, refunds);
        console.log("cumulative: reverts caught %d", reverts);

        if (calls >= MIN_CALLS) {
            assertGt(debits, 0, "no successful debit in the campaign");
            assertGt(refunds, 0, "no successful refund in the campaign");
        }
    }

    function _accumulate(string memory key, uint256 value) internal returns (uint256 total) {
        total = vm.envOr(key, uint256(0)) + value;
        vm.setEnv(key, vm.toString(total));
    }
}
