# Test Plan — S1 Contracts

- **Milestone:** S1 — Contracts, version `v0.1.0`, branch `feature/v0.1.0`.
- **Objects under test:** `CardSpendController`, `MockUSDC` ([SRS — Card Spend](srs/card-spend.md) §2.1.5), the Anvil deployment script, the ABI export.
- **Parents:** SRS — Card Spend §2.1.5, FR-6, FR-12, FR-13; [BRD](brd.md) BR-6, BR-8, BR-9, BR-10, G-3; [ADR-8](adr/0008-network-and-token.md), [ADR-9](adr/0009-card-spend-controller.md), [ADR-12](adr/0012-authorization-decision-timing.md); [SRS — EVM Connector](srs/evm-connector.md) §2.1.1, §2.1.2.
- **Status:** Signed off 2026-10-04 (§6). Created in the discovery stage of S1, completed stage by stage.

## 1. Environment

| Item | Value |
|---|---|
| Chain | Anvil, chain ID 31337. No public network |
| Foundry | `v1.8.3` (forge, cast, anvil; 2026-09-15). Same version locally and in CI |
| Solidity | `0.8.37`, exact pin; `evm_version = cancun` |
| OpenZeppelin Contracts | `v5.6.1`, git submodule in `contracts/lib/` |
| forge-std | `v1.17.0`, git submodule in `contracts/lib/` |
| Keys | Anvil's unlocked default accounts. No private key in the repository |
| Runs | Local profile: fuzz 256, invariant 256 runs × depth 15. CI profile: fuzz 1 000, invariant 500 runs × depth 50 |

## 2. Conventions

- Row format: `ID | Description | Preconditions | Steps | Expected | Req | Status`. `Req` names the requirement IDs the row covers.
- ID `S1-Tnnn`; the hundreds digit is the phase. A Foundry test names its row: `test_T302_debit_revertsOnUsedAuthId`.
- `Status`: `—` not run, `Pass`, `Fail <attempt>`, `Blocked`. Filled in the QA stage (§5).
- Severity of a failure: P0 funds can move wrongly or be stuck; P1 a rule of §2.1.5 is not enforced; P2 event, view or script defect; P3 cosmetic.
- Common preconditions for `debit` rows: a limit set for `user`; `user` holds tokens and approved the controller; `validUntil` in the future; caller is `operator`. For `refund` rows: a debit `(authId, user, D)` exists; the treasury holds tokens and approved the controller; caller is `operator`.

## 3. Phases

| Phase | Scope | Stage |
|---|---|---|
| 1 | `MockUSDC` | st3 |
| 2 | Constructor, roles, views | st4 |
| 3 | `debit` | st4 |
| 4 | `refund` | st5 |
| 5 | `setDailyLimit`, `pause`, `unpause` | st5 |
| 6 | Fuzz and invariants | st6 |
| 7 | Deployment script, scenario, ABI | st7 |

## 4. Test matrix

### Phase 1 — `MockUSDC`

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S1-T101 | Metadata | Deployed | Read `name`, `symbol`, `decimals` | `Mock USDC`, `USDC`, 6 | ADR-8, §2.1.5 | Pass |
| S1-T102 | Open mint | Deployed | A stranger calls `mint(to, amount)` | Balance and `totalSupply` grow by `amount`; `Transfer(0x0, to, amount)` | ADR-8, EVM §2.1.1 | Pass |
| S1-T103 | Burn | Holder has balance | `burn(amount)`; `burnFrom` with allowance | Balance and `totalSupply` fall; `Transfer(from, 0x0, amount)` | EVM §2.1.1 | Pass |
| S1-T104 | Plain ERC-20 | Two accounts, balance | `approve`, `transferFrom`, `transfer` | Balances change by exactly `amount`; allowance decreases; no fee | EVM §2.1.1 | Pass |

### Phase 2 — Constructor, roles, views

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S1-T201 | Immutables | — | Deploy with `token`, `treasury`, `admin`, `operator` | `token()` and `treasury()` return the constructor values | §2.1.5 | Pass |
| S1-T202 | Roles at deployment | — | Deploy | `admin` holds `DEFAULT_ADMIN_ROLE`, `operator` holds `OPERATOR_ROLE`; nobody else holds either | §2.1.5, ADR-9 | Pass |
| S1-T203 | Zero constructor argument | — | Deploy with a zero `token`, `treasury`, `admin` or `operator`, one at a time | Reverts `ZeroAddress` | §2.1.5 | Pass |
| S1-T204 | `debit` by a non-operator | Deployed | Call from `admin`, from a stranger | Reverts `AccessControlUnauthorizedAccount` | ADR-9 | Pass |
| S1-T205 | `refund` by a non-operator | A debit exists | Call from `admin`, from a stranger | Reverts `AccessControlUnauthorizedAccount` | ADR-9 | Pass |
| S1-T206 | `setDailyLimit` by a non-admin | Deployed | Call from `operator`, from a stranger | Reverts `AccessControlUnauthorizedAccount` | ADR-9, BR-9 | Pass |
| S1-T207 | `pause` / `unpause` by a non-admin | Deployed | Call from `operator`, from a stranger | Reverts `AccessControlUnauthorizedAccount` | ADR-9, BR-9 | Pass |
| S1-T208 | Admin rotates the operator | Deployed | `admin` grants `OPERATOR_ROLE` to a new key, revokes the old one | New key can `debit`; old key reverts | §2.1.5, ADR-9 | Pass |
| S1-T209 | Contract holds nothing at start | Deployed | `token.balanceOf(controller)` | 0 | G-3, BR-6 | Pass |
| S1-T210 | Unknown `authId` view | Deployed | `authorizations(random)` | `(address(0), 0, 0)` | §2.1.5 | Pass |
| S1-T211 | `remainingDailyLimit` without a limit | Deployed | Read for a fresh user | 0 | §2.1.5 | Pass |
| S1-T212 | `refundUsed` before any refund | Deployed | Read for a random `refundId` | `false` | §2.1.5 | Pass |

### Phase 3 — `debit`

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S1-T301 | Happy path | Common | `debit(user, amount, authId, validUntil)` | `user` −amount, `treasury` +amount, controller 0; `Debited(authId, user, amount)` with topics 1 = `authId`, 2 = `user`; `authorizations(authId) == (user, amount, 0)`; `remainingDailyLimit(user)` falls by `amount` | §2.1.5, BR-6, EVM §2.1.2 | Pass |
| S1-T302 | `authId` reused | A debit with `authId` exists | Same `authId`, any user or amount | Reverts `AuthAlreadyUsed`; no transfer | §2.1.5, BR-8 | Pass |
| S1-T303 | Expired | Common | Warp to `validUntil + 1`; `debit` | Reverts `AuthExpired`; no transfer | §2.1.5, FR-6, ADR-12 | Pass |
| S1-T304 | Boundary `block.timestamp == validUntil` | Common | Warp to exactly `validUntil`; `debit` | Passes | §2.1.5 | Pass |
| S1-T305 | Zero amount | Common | `debit(user, 0, …)` | Reverts `ZeroAmount` | §2.1.5 | Pass |
| S1-T306 | Over the daily limit | Limit `L`; spent today `S` | `debit` with `S + amount > L`; then with `S + amount == L` | First reverts `DailyLimitExceeded`; second passes | §2.1.5, BR-9 | Pass |
| S1-T307 | Default limit 0 | No `setDailyLimit` for `user` | `debit(user, 1, …)` | Reverts `DailyLimitExceeded` | §2.1.5 | Pass |
| S1-T308 | Paused | `admin` called `pause()` | `debit` | Reverts `EnforcedPause`; no transfer | §2.1.5, BR-9 | Pass |
| S1-T309 | Allowance too low | Allowance < amount | `debit` | Reverts with the token's `ERC20InsufficientAllowance`; `authorizations(authId)` stays empty | §2.1.5 | Pass |
| S1-T310 | Balance too low | Balance < amount ≤ allowance | `debit` | Reverts with the token's `ERC20InsufficientBalance`; `authorizations(authId)` stays empty | §2.1.5 | Pass |
| S1-T311 | UTC day rollover | Limit `L`, spent `L` at day `D` | `debit` at `D 23:59:59`; warp to `(D+1) 00:00:00`; `debit(L)` | First reverts `DailyLimitExceeded`; second passes; `remainingDailyLimit == L` at the day start | §2.1.5, BR-9 | Pass |
| S1-T312 | Refund does not restore the limit | Spent `L` today; part refunded | `debit` again today | Reverts `DailyLimitExceeded`; `remainingDailyLimit == 0` | §2.1.5, ADR-9 | Pass |
| S1-T313 | Limits are per wallet | Users A and B with limits | A spends its limit | B's `remainingDailyLimit` unchanged; B can `debit` | BR-9 | Pass |
| S1-T314 | Zero `user` | — | `debit(address(0), amount, authId, validUntil)` | Reverts `ZeroAddress`; `authorizations(authId)` stays empty | §2.1.5 | Pass |
| S1-T315 | Zero `authId` | Common | `debit(user, amount, bytes32(0), validUntil)` | Passes like any other `authId`; a second use reverts `AuthAlreadyUsed` | §2.1.5 | Pass |
| S1-T316 | Several debits, same user, same day | Limit `L` | Debits summing to `L` with distinct `authId`; one more | All pass; the last reverts `DailyLimitExceeded`; each `authId` recorded separately | §2.1.5 | Pass |
| S1-T317 | Check order | Paused, zero amount, expired, used `authId` at once | `debit` with several failing rules | The error of the first failing check in the order of §2.1.5: role, pause, `ZeroAmount`, `ZeroAddress`, `AuthExpired`, `AuthAlreadyUsed`, `DailyLimitExceeded` | §2.1.5 | Pass |

### Phase 4 — `refund`

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S1-T401 | Partial refund | Common | `refund(authId, refundId, a)`, `a < D` | `treasury` −a, `user` +a, controller 0; `Refunded(authId, refundId, user, a)` with topics 1, 2, 3 = `authId`, `refundId`, `user`; `refunded == a`; `refundUsed(refundId) == true` | §2.1.5, BR-10, EVM §2.1.2 | Pass |
| S1-T402 | Full refund | Common | `refund(authId, refundId, D)` | `refunded == debited`; `user` balance restored exactly | FR-12 | Pass |
| S1-T403 | Partial refunds up to the debit | Common | Refunds `a1 + a2 + a3 == D`, distinct `refundId`; then one more of 1 unit | Three pass; the fourth reverts `RefundExceedsDebit` | FR-12, BR-10 | Pass |
| S1-T404 | Unknown `authId` | — | `refund(random, refundId, a)` | Reverts `UnknownAuth`; no transfer | §2.1.5 | Pass |
| S1-T405 | `refundId` reused | A refund with `refundId` exists | Same `refundId` with the same and with another `authId` | Reverts `RefundAlreadyUsed`; no transfer | §2.1.5, FR-13, BR-10 | Pass |
| S1-T406 | Over the debit in one call | Common | `refund(authId, refundId, D + 1)` | Reverts `RefundExceedsDebit` | §2.1.5, FR-12 | Pass |
| S1-T407 | Refund while paused | `admin` called `pause()` | `refund` | Passes; event emitted | §2.1.5, ADR-9 | Pass |
| S1-T408 | Zero refund amount | Common | `refund(authId, refundId, 0)` | Reverts `ZeroAmount`; `refundUsed(refundId)` stays `false` | §2.1.5 | Pass |
| S1-T409 | Recipient fixed by the debit | Common | `refund` | Tokens arrive at the `user` stored for `authId`; the function has no recipient parameter | §2.1.5, ADR-9 | Pass |
| S1-T410 | Treasury cannot fund | Treasury allowance or balance < a | `refund` | Reverts with the token's error; `refundUsed(refundId)` stays `false`; `refunded` unchanged | §2.1.5, EC-12 | Pass |
| S1-T411 | Check order | Zero amount, unknown `authId`, used `refundId` at once | `refund` | The error of the first failing check: `ZeroAmount`, `UnknownAuth`, `RefundAlreadyUsed`, `RefundExceedsDebit` | §2.1.5 | Pass |

### Phase 5 — Admin

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S1-T501 | Set a limit | Deployed | `admin` calls `setDailyLimit(user, L)` | `DailyLimitSet(user, L)` with topic 1 = `user`; `remainingDailyLimit(user) == L` | §2.1.5, BR-9 | Pass |
| S1-T502 | Lower the limit below today's spend | Spent `S` today; `L' < S` | `setDailyLimit(user, L')`; read the view; `debit(1)` | View returns 0 without revert; `debit` reverts `DailyLimitExceeded`; balances unchanged | §2.1.5 | Pass |
| S1-T503 | Raise the limit mid-day | Spent `S`, limit `L` | `setDailyLimit(user, L2 > L)` | `remainingDailyLimit == L2 − S`; a debit of `L2 − S` passes | §2.1.5 | Pass |
| S1-T504 | Limit 0 stops spending | Limit `L`, nothing spent | `setDailyLimit(user, 0)`; `debit(1)` | Reverts `DailyLimitExceeded` | BR-9 | Pass |
| S1-T505 | Pause and unpause | Deployed | `pause()`; `unpause()` | `Paused(admin)`, `Unpaused(admin)`; `paused()` toggles | §2.1.5 | Pass |
| S1-T506 | Double pause / unpause | — | `pause()` twice; `unpause()` when not paused | Reverts `EnforcedPause` / `ExpectedPause` | §2.1.5 | Pass |
| S1-T507 | Debit after unpause | Paused, then unpaused | `debit` | Passes | BR-9 | Pass |

### Phase 6 — Fuzz and invariants

Handler-based suite. Actors: `operator`, `admin`, several users, `treasury`. The handler mints, approves, sets limits, debits with fresh and reused IDs, refunds, pauses, unpauses and warps time; it records ghost values for the assertions.

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S1-T601 | Invariant: controller balance | Handler | Any sequence | `token.balanceOf(controller) == 0` after every call | G-3, BR-6 | Pass |
| S1-T602 | Invariant: refunded ≤ debited | Handler | Any sequence | For every recorded `authId`: `refunded <= debited` | FR-12, BR-10 | Pass |
| S1-T603 | Invariant: `authId` single-use | Handler ghost set | Any sequence | A second `debit` with a recorded `authId` always reverts; `debited` and `user` of a recorded `authId` never change | BR-8 | Pass |
| S1-T604 | Invariant: `refundId` single-use | Handler ghost set | Any sequence | A second `refund` with a recorded `refundId` always reverts; `refundUsed` never flips back | FR-13, BR-10 | Pass |
| S1-T605 | Invariant: conservation | Handler ghost sums | Any sequence | `treasury balance == initial + Σ debited − Σ refunded`, counting only the controller's movements | BR-6 | Pass |
| S1-T606 | Invariant: daily spend within limit | Handler | Any sequence | For every user and UTC day: Σ debits of that day ≤ the limit in force at the last debit of that day | BR-9 | Pass |
| S1-T607 | Fuzz: expiry | — | Random `validUntil`, random warp | `debit` passes iff `block.timestamp <= validUntil` | FR-6 | Pass |
| S1-T608 | Fuzz: day arithmetic | — | Random timestamps around day boundaries | `remainingDailyLimit` resets exactly at `ts / 1 days` boundaries | §2.1.5 | Pass |

### Phase 7 — Deployment and ABI

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S1-T701 | Script on a fresh Anvil | Anvil running, chain ID 31337 | Run the deployment script | `MockUSDC` then the controller; `token()` is the mock; `treasury()`, `admin`, `operator` as given; no key material in the repository | Package 7.1, handoff §4 | Pass |
| S1-T702 | Full-cycle scenario | T701 done | mint → approve → `setDailyLimit` → `debit` → partial `refund` | Wallet, treasury and controller balances as expected; controller 0 | Package 7.2 | Pass |
| S1-T703 | ABI export | Build passes | Export both ABIs to `contracts/abi/` | Files committed; CI shows the committed ABI equals the build output | Package 7.3, 7.4 | Pass |
| S1-T704 | Chain guard | — | Run the scripts against a chain ID other than 31337; grep scripts and configuration | Scripts revert; no mainnet chain ID or RPC in the repository | Handoff §4 | Pass |

### Coverage of the exit criteria of S1

| Criterion | Rows |
|---|---|
| Every revert rule of §2.1.5 has a test | T203, T302 … T310, T314, T404 … T406, T408 |
| Contract token balance is 0 after every operation | T209, T301, T401, T601, T702 |
| A used `authId` or `refundId` cannot be used again | T302, T405, T603, T604 |
| Total refunds never exceed the debit | T403, T406, T602 |
| A debit after `validUntil` reverts | T303, T607 |
| The daily limit works by UTC day | T311, T608 |
| Refunds work while paused | T407 |

## 5. Run log

Batches run in the QA stage in phase order; one line per batch and attempt. Cycle: fix → retest → regression of the earlier phases.

| Date | Batch | Attempt | Pass | Fail | Failed IDs | Notes |
|---|---|---|---|---|---|---|
| 2026-10-04 | 1 `MockUSDC` | 1 | 4 | 0 | — | — |
| 2026-10-04 | 2 Setup | 1 | 12 | 0 | — | — |
| 2026-10-04 | 3 `debit` | 1 | 17 | 0 | — | — |
| 2026-10-04 | 4 `refund` | 1 | 11 | 0 | — | — |
| 2026-10-04 | 5 Admin | 1 | 7 | 0 | — | — |
| 2026-10-04 | 6 Fuzz and invariants, CI profile | 1 | 8 | 0 | — | 2 fuzz × 1 000 runs; 6 invariants in one campaign of 500 × 50, about 3.2 s |
| 2026-10-04 | 7 Scripts and ABI | 1 | 4 | 0 | — | T701, T702, T704 in-process; T701, T702 also on a live Anvil in st7; T703 by the ABI diff |
| 2026-10-04 | Regression, CI profile | 1 | 63 | 0 | — | `forge test`: 57 passed (the invariant suite counts as 1); ABI diff empty; CI run green on `47f37be` |

## 6. Success criteria and sign-off

- Every row of §4 is `Pass` with the CI profile, locally and in CI.
- 0 open P0 and P1. Open P2 and P3 are listed in `known-issues.md` with their row IDs.
- The ABI in `contracts/abi/` equals the build output of the signed-off commit.
- `docs/srs/card-spend.md` §2.1.5 matches the code.

| Role | Name | Date | Result |
|---|---|---|---|
| Owner, QA | Igor Kudinov | 2026-10-04 | 63 of 63 rows `Pass`; 0 open P0–P3 |

## 7. Notes of the QA stage

- `forge lint` reports 4 warnings on `CardSpendController`: `arbitrary-send-erc20` on the two `safeTransferFrom` calls and `block-timestamp` on the expiry check and the day calculation. All four are the design of §2.1.5 and ADR-9. Not defects; lint is not a CI step.
- Mutation check of the invariant suite (st6): four temporary mutations of the contract — over-refund allowed, `authId` reuse allowed, limit check removed, `refundId` not recorded — were each caught by the matching invariants. The contract was restored unchanged.
- Live Anvil run of st7: `Deploy` from account 0, `Scenario` with accounts 1–4 as admin, operator, treasury and wallet; the on-chain values read back with `cast call` equalled the script's checks.
- Known issues and backlog: none opened in S1.
- Since S2 (owner's decision D-3 of 2026-10-05) the scripts also accept chain ID 84532, Base Sepolia. The test of S1-T704 was changed with it: chain IDs 1, 8453 and 11155111 are refused; 84532 is covered by S2-T801 in `docs/test-plan-s2.md`.
