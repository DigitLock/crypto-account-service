# Contracts

`CardSpendController` and `MockUSDC` of milestone S1. Interface: [SRS — Card Spend §2.1.5](../docs/srs/card-spend.md). Tests: [test plan S1](../docs/test-plan-s1.md).

## Toolchain

| Item | Pin |
|---|---|
| Foundry | `v1.8.3`, locally and in CI |
| Solidity | `0.8.37`, exact; `evm_version = cancun` |
| OpenZeppelin Contracts | `v5.6.1`, submodule `lib/openzeppelin-contracts` |
| forge-std | `v1.17.0`, submodule `lib/forge-std` |

- After cloning: `git submodule update --init --recursive`.

## Commands

Run from `contracts/`.

| Task | Command |
|---|---|
| Build | `forge build` |
| Test | `forge test -vvv` |
| Test with CI run counts | `FOUNDRY_PROFILE=ci forge test` |
| Format | `forge fmt` |
| Format check | `forge fmt --check` |

## Local deployment on Anvil

- The scripts refuse any chain other than 31337 (local Anvil). Deployment to Base Sepolia is S2.
- No private key and no mnemonic is used: scripts sign with Anvil's unlocked accounts (`--unlocked --sender`).
- Take the addresses from the `Available Accounts` list of the anvil banner.

| Index | Role | Env variable | Default Anvil address |
|---|---|---|---|
| 0 | Deployer | `DEPLOYER` | `0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266` |
| 1 | Admin | `ADMIN` | `0x70997970C51812dc3A010C7d01b50e0d17dc79C8` |
| 2 | Operator | `OPERATOR` | `0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC` |
| 3 | Treasury | `TREASURY` | `0x90F79bf6EB2c4f870365E785982E1f101E93b906` |
| 4 | Cardholder wallet | `USER` | `0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65` |

Steps:

1. Start Anvil in a separate terminal: `anvil --chain-id 31337`.
2. Export `DEPLOYER`, `ADMIN`, `OPERATOR`, `TREASURY` and `USER` with the addresses of the table.
3. Deploy. `TOKEN` unset deploys `MockUSDC` first; set it to reuse a deployed token.
   ```sh
   forge script script/Deploy.s.sol --rpc-url http://127.0.0.1:8545 --broadcast --unlocked --sender $DEPLOYER
   ```
4. Export `TOKEN` and `CONTROLLER` from the `TOKEN=` and `CONTROLLER=` lines of the output.
5. Run the full-cycle scenario (test plan row S1-T702) on the fresh pair. Each step broadcasts as its actor; the script reverts if a check fails.
   ```sh
   forge script script/Scenario.s.sol --rpc-url http://127.0.0.1:8545 --broadcast --unlocked --sender $USER
   ```

| Scenario step | Actor | Amount |
|---|---|---|
| `mint` to self, `approve` the controller | `USER` | 100.00 USDC |
| `setDailyLimit(USER)` | `ADMIN` | 50.00 USDC |
| `debit`, `authId = keccak256("scenario-auth-1")` | `OPERATOR` | 25.40 USDC |
| `approve` the controller | `TREASURY` | 10.00 USDC |
| `refund`, `refundId = keccak256("scenario-refund-1")` | `OPERATOR` | 10.00 USDC |
| Expected: `USER` 84.60, `TREASURY` 15.40, controller 0, remaining limit 24.60 | — | — |

- `broadcast/`, `cache/` and `out/` are ignored by git.

## ABI

| Contract | File |
|---|---|
| `CardSpendController` | [`abi/CardSpendController.json`](abi/CardSpendController.json) |
| `MockUSDC` | [`abi/MockUSDC.json`](abi/MockUSDC.json) |

- Exported with `forge inspect <Contract> abi --json > abi/<Contract>.json`.
- Frozen after S1: a change needs the owner's decision.
- CI step `ABI frozen` fails when the build output differs from these files.
