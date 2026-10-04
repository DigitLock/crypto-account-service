// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

// Pulls card spend from a cardholder wallet to the treasury under an ERC-20 allowance (ADR-9).
// The contract never holds tokens. Token and treasury are immutable. authId is single-use;
// a per-wallet daily limit applies by UTC day.

import {AccessControl} from "@openzeppelin/contracts/access/AccessControl.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {Pausable} from "@openzeppelin/contracts/utils/Pausable.sol";

contract CardSpendController is AccessControl, Pausable {
    using SafeERC20 for IERC20;

    bytes32 public constant OPERATOR_ROLE = keccak256("OPERATOR_ROLE");

    IERC20 public immutable token;
    address public immutable treasury;

    struct Authorization {
        address user;
        uint256 debited;
        uint256 refunded;
    }

    mapping(bytes32 => Authorization) public authorizations;
    mapping(bytes32 => bool) public refundUsed;

    mapping(address => uint256) internal _dailyLimit;
    // user => UTC day (block.timestamp / 1 days) => amount debited that day
    mapping(address => mapping(uint256 => uint256)) internal _spent;

    event Debited(bytes32 indexed authId, address indexed user, uint256 amount);
    event Refunded(bytes32 indexed authId, bytes32 indexed refundId, address indexed user, uint256 amount);
    event DailyLimitSet(address indexed user, uint256 limit);

    error ZeroAddress();
    error ZeroAmount();
    error AuthExpired();
    error AuthAlreadyUsed();
    error DailyLimitExceeded();
    error UnknownAuth();
    error RefundAlreadyUsed();
    error RefundExceedsDebit();

    constructor(IERC20 token_, address treasury_, address admin, address operator) {
        if (address(token_) == address(0) || treasury_ == address(0) || admin == address(0) || operator == address(0)) {
            revert ZeroAddress();
        }
        token = token_;
        treasury = treasury_;
        _grantRole(DEFAULT_ADMIN_ROLE, admin);
        _grantRole(OPERATOR_ROLE, operator);
    }

    /// @notice Pulls `amount` from `user` to the treasury once per `authId`, within the daily limit and before `validUntil`.
    function debit(address user, uint256 amount, bytes32 authId, uint64 validUntil)
        external
        onlyRole(OPERATOR_ROLE)
        whenNotPaused
    {
        if (amount == 0) revert ZeroAmount();
        if (user == address(0)) revert ZeroAddress();
        if (block.timestamp > validUntil) revert AuthExpired();
        if (authorizations[authId].user != address(0)) revert AuthAlreadyUsed();
        uint256 day = block.timestamp / 1 days;
        if (amount > _remaining(user, day)) revert DailyLimitExceeded();

        _spent[user][day] += amount;
        authorizations[authId] = Authorization(user, amount, 0);
        emit Debited(authId, user, amount);
        token.safeTransferFrom(user, treasury, amount);
    }

    /// @notice Returns `amount` of debit `authId` from the treasury to its stored user once per `refundId`; works while paused.
    function refund(bytes32 authId, bytes32 refundId, uint256 amount) external onlyRole(OPERATOR_ROLE) {
        if (amount == 0) revert ZeroAmount();
        Authorization storage auth = authorizations[authId];
        address user = auth.user;
        if (user == address(0)) revert UnknownAuth();
        if (refundUsed[refundId]) revert RefundAlreadyUsed();
        if (amount > auth.debited - auth.refunded) revert RefundExceedsDebit();

        refundUsed[refundId] = true;
        auth.refunded += amount;
        emit Refunded(authId, refundId, user, amount);
        token.safeTransferFrom(treasury, user, amount);
    }

    /// @notice Sets the daily limit of `user` in token base units.
    function setDailyLimit(address user, uint256 limit) external onlyRole(DEFAULT_ADMIN_ROLE) {
        _dailyLimit[user] = limit;
        emit DailyLimitSet(user, limit);
    }

    /// @notice Blocks debits. Refunds stay available.
    function pause() external onlyRole(DEFAULT_ADMIN_ROLE) {
        _pause();
    }

    /// @notice Allows debits again.
    function unpause() external onlyRole(DEFAULT_ADMIN_ROLE) {
        _unpause();
    }

    /// @notice Daily limit of `user` minus today's spend; 0 if today's spend already exceeds the limit.
    function remainingDailyLimit(address user) external view returns (uint256) {
        return _remaining(user, block.timestamp / 1 days);
    }

    // Equivalent to `spent + amount > limit` in debit, without an overflow on a huge amount.
    function _remaining(address user, uint256 day) internal view returns (uint256) {
        uint256 limit = _dailyLimit[user];
        uint256 spent = _spent[user][day];
        return spent >= limit ? 0 : limit - spent;
    }
}
