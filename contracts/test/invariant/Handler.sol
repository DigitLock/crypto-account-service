// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {CardSpendController} from "../../src/CardSpendController.sol";
import {MockUSDC} from "../../src/MockUSDC.sol";
import {IERC20Errors} from "@openzeppelin/contracts/interfaces/draft-IERC6093.sol";
import {Pausable} from "@openzeppelin/contracts/utils/Pausable.sol";
import {CommonBase} from "forge-std/Base.sol";
import {StdUtils} from "forge-std/StdUtils.sol";

// Actor of the Phase 6 invariant suite (docs/test-plan-s1.md). Every controller call runs inside try/catch:
// with fail_on_revert = false a reverting handler call would be dropped silently, so a broken expectation is
// recorded in a ghost counter instead, and the invariants assert that the counters stay 0.
contract Handler is CommonBase, StdUtils {
    struct DayRecord {
        address user;
        uint256 day;
    }

    CardSpendController public immutable controller;
    MockUSDC public immutable token;
    address public immutable admin;
    address public immutable operator;
    address public immutable treasury;
    address[4] internal users;

    // Ghost state.
    uint256 public immutable treasuryInitial;
    bytes32[] public authIds;
    mapping(bytes32 => address) public ghostUser;
    mapping(bytes32 => uint256) public ghostDebited;
    mapping(bytes32 => uint256) public ghostRefunded;
    bytes32[] public refundIds;
    bytes32[] public replayedRefundIds;
    mapping(address => uint256) public ghostLimit;
    mapping(address => mapping(uint256 => uint256)) public daySpend;
    mapping(address => mapping(uint256 => uint256)) public limitAtLastDebit;
    DayRecord[] internal dayRecords;
    uint256 public sumDebited;
    uint256 public sumRefunded;

    // Counters of this run.
    uint256 public calls;
    uint256 public debits;
    uint256 public refunds;
    uint256 public revertsCaught;

    // Broken expectations; the invariants require 0.
    uint256 public debitMismatches;
    uint256 public debitReplaysAccepted;
    uint256 public refundMismatches;
    uint256 public refundReplaysAccepted;

    uint256 internal nonce;

    constructor(
        CardSpendController controller_,
        MockUSDC token_,
        address admin_,
        address operator_,
        address treasury_,
        address[4] memory users_
    ) {
        controller = controller_;
        token = token_;
        admin = admin_;
        operator = operator_;
        treasury = treasury_;
        users = users_;
        treasuryInitial = token_.balanceOf(treasury_);
    }

    // Actions

    function mint(uint256 userSeed, uint256 amount) public {
        calls++;
        address user = _pick(userSeed, true, false);
        amount = bound(amount, 1, 1_000_000e6);
        token.mint(user, amount);
        // Read the balance before the prank: a call in the arguments would consume it.
        uint256 balance = token.balanceOf(user);
        vm.prank(user);
        token.approve(address(controller), balance);
    }

    function setLimit(uint256 userSeed, uint256 limit) public {
        calls++;
        address user = _pick(userSeed, false, true);
        limit = bound(limit, 0, 2_000_000e6);
        vm.prank(admin);
        controller.setDailyLimit(user, limit);
        ghostLimit[user] = limit;
    }

    function debitFresh(uint256 userSeed, uint256 amount, uint256 authSeed) public {
        calls++;
        address user = _pick(userSeed, true, true);
        uint256 day = _today();
        uint256 remaining = _ghostRemaining(user, day);
        uint256 allowance = token.allowance(user, address(controller));
        uint256 balance = token.balanceOf(user);
        // Mostly affordable amounts, with 0 and cap + 1 reachable as edge values.
        uint256 cap = _min(remaining, _min(allowance, balance));
        amount = bound(amount, 0, cap + 1);
        bytes32 authId = keccak256(abi.encode("auth", authSeed, ++nonce));
        uint64 validUntil = uint64(vm.getBlockTimestamp() + 60);

        bytes4 expected;
        if (controller.paused()) expected = Pausable.EnforcedPause.selector;
        else if (amount == 0) expected = CardSpendController.ZeroAmount.selector;
        else if (amount > remaining) expected = CardSpendController.DailyLimitExceeded.selector;
        else if (amount > allowance) expected = IERC20Errors.ERC20InsufficientAllowance.selector;
        else if (amount > balance) expected = IERC20Errors.ERC20InsufficientBalance.selector;

        vm.prank(operator);
        try controller.debit(user, amount, authId, validUntil) {
            if (expected != bytes4(0)) {
                debitMismatches++;
                return;
            }
            authIds.push(authId);
            ghostUser[authId] = user;
            ghostDebited[authId] = amount;
            if (daySpend[user][day] == 0) dayRecords.push(DayRecord(user, day));
            daySpend[user][day] += amount;
            limitAtLastDebit[user][day] = ghostLimit[user];
            sumDebited += amount;
            debits++;
        } catch (bytes memory reason) {
            if (expected == bytes4(0) || bytes4(reason) != expected) debitMismatches++;
            else revertsCaught++;
        }
    }

    function debitReplay(uint256 index, uint256 userSeed, uint256 amount) public {
        calls++;
        if (authIds.length == 0) return;
        bytes32 authId = authIds[bound(index, 0, authIds.length - 1)];
        amount = bound(amount, 1, 2_000_000e6);
        // The pause check comes before the authId check.
        bytes4 expected =
            controller.paused() ? Pausable.EnforcedPause.selector : CardSpendController.AuthAlreadyUsed.selector;

        vm.prank(operator);
        try controller.debit(_user(userSeed), amount, authId, uint64(vm.getBlockTimestamp() + 60)) {
            debitReplaysAccepted++;
        } catch (bytes memory reason) {
            if (bytes4(reason) != expected) debitReplaysAccepted++;
            else revertsCaught++;
        }
    }

    function refundFresh(uint256 index, uint256 amount, uint256 refundSeed) public {
        calls++;
        if (authIds.length == 0) return;
        bytes32 authId = authIds[bound(index, 0, authIds.length - 1)];
        uint256 left = ghostDebited[authId] - ghostRefunded[authId];
        amount = bound(amount, 1, left + 1);
        bytes32 refundId = keccak256(abi.encode("refund", refundSeed, ++nonce));

        if (token.allowance(treasury, address(controller)) < amount) {
            uint256 balance = token.balanceOf(treasury);
            vm.prank(treasury);
            token.approve(address(controller), balance);
        }

        vm.prank(operator);
        try controller.refund(authId, refundId, amount) {
            if (amount > left) {
                refundMismatches++;
                return;
            }
            refundIds.push(refundId);
            ghostRefunded[authId] += amount;
            sumRefunded += amount;
            refunds++;
        } catch (bytes memory reason) {
            if (amount <= left || bytes4(reason) != CardSpendController.RefundExceedsDebit.selector) {
                refundMismatches++;
            } else {
                revertsCaught++;
            }
        }
    }

    function refundReplay(uint256 refundIndex, uint256 authIndex, uint256 amount) public {
        calls++;
        if (refundIds.length == 0) return;
        bytes32 refundId = refundIds[bound(refundIndex, 0, refundIds.length - 1)];
        bytes32 authId = authIds[bound(authIndex, 0, authIds.length - 1)];
        amount = bound(amount, 1, 2_000_000e6);
        replayedRefundIds.push(refundId);

        vm.prank(operator);
        try controller.refund(authId, refundId, amount) {
            refundReplaysAccepted++;
        } catch (bytes memory reason) {
            if (bytes4(reason) != CardSpendController.RefundAlreadyUsed.selector) refundReplaysAccepted++;
            else revertsCaught++;
        }
    }

    function pause() public {
        calls++;
        if (controller.paused()) return;
        vm.prank(admin);
        controller.pause();
    }

    function unpause() public {
        calls++;
        if (!controller.paused()) return;
        vm.prank(admin);
        controller.unpause();
    }

    function warp(uint256 secs) public {
        calls++;
        secs = bound(secs, 1, 3 days);
        vm.warp(vm.getBlockTimestamp() + secs);
    }

    // Views for the invariants

    function userAt(uint256 i) external view returns (address) {
        return users[i];
    }

    function authIdCount() external view returns (uint256) {
        return authIds.length;
    }

    function refundIdCount() external view returns (uint256) {
        return refundIds.length;
    }

    function replayedRefundIdCount() external view returns (uint256) {
        return replayedRefundIds.length;
    }

    function dayRecordCount() external view returns (uint256) {
        return dayRecords.length;
    }

    function dayRecordAt(uint256 i) external view returns (address user, uint256 day) {
        DayRecord memory r = dayRecords[i];
        return (r.user, r.day);
    }

    function ghostRemainingToday(address user) external view returns (uint256) {
        return _ghostRemaining(user, _today());
    }

    // Internals

    function _user(uint256 seed) internal view returns (address) {
        return users[seed % users.length];
    }

    // Actor selection: from the seeded user on, the first one that meets the need; else the seeded user.
    // A successful debit needs mint, a non-zero setLimit and debitFresh for the same user within one run;
    // with 4 users and a depth of 15 that is rare unless the actions steer towards each other:
    // mint prefers a user with limit left, setLimit a funded user, debitFresh a user who can spend now.
    // An action that finds no such user uses the seeded one, and its expected outcome is checked as usual.
    function _pick(uint256 seed, bool needLimit, bool needFunds) internal view returns (address) {
        uint256 day = _today();
        for (uint256 i; i < users.length; ++i) {
            address user = users[(seed % users.length + i) % users.length];
            bool limitOk = !needLimit || _ghostRemaining(user, day) > 0;
            bool fundsOk = !needFunds || (token.balanceOf(user) > 0 && token.allowance(user, address(controller)) > 0);
            if (limitOk && fundsOk) return user;
        }
        return _user(seed);
    }

    function _today() internal view returns (uint256) {
        return vm.getBlockTimestamp() / 1 days;
    }

    function _ghostRemaining(address user, uint256 day) internal view returns (uint256) {
        uint256 limit = ghostLimit[user];
        uint256 spent = daySpend[user][day];
        return spent >= limit ? 0 : limit - spent;
    }

    function _min(uint256 a, uint256 b) internal pure returns (uint256) {
        return a < b ? a : b;
    }
}
