#!/usr/bin/env bash
# deploy.sh: deploys MockUSDC and CardSpendController on Base Sepolia with contracts/script/Deploy.s.sol
# (docs/deployment-guide.md step 1; test plan S2-T802).
#
# What it does:
#   1. checks eth_chainId of CAS_SEPOLIA_RPC_URL (84532 only), then the keys file of CAS_SEPOLIA_KEYS;
#   2. derives the five addresses of the keys file and prints their ETH balances;
#   3. runs Deploy.s.sol without TOKEN, so MockUSDC is deployed first; broadcast by DEPLOYER; roles TREASURY,
#      ADMIN, OPERATOR;
#   4. checks token(), treasury(), decimals() and both roles on chain;
#   5. prints addresses, transaction hashes and explorer links, and writes them to cas-sepolia-deployment.env
#      next to the keys file. forge output goes to cas-sepolia-forge/ there, never into the repository.
# Needs: forge, cast, jq, perl; ETH of DEPLOYER for gas. Refuses to run when the deployment file exists.
# Variables: CAS_SEPOLIA_KEYS (required); CAS_SEPOLIA_RPC_URL (default CARD_AUTH_RPC_URL of the
#   environment file when set, else https://sepolia.base.org);
#   CAS_SEPOLIA_REHEARSAL=1 for a rehearsal on a local Anvil with chain ID 84532.

. "$(dirname "$0")/lib.sh"

need forge jq
select_rpc
start_checks "$RPC_NAME" "$RPC"
[ ! -e "$DEPLOYMENT_FILE" ] || die "cas-sepolia-deployment.env exists next to the keys file: move it away to deploy again"

for role in DEPLOYER ADMIN OPERATOR TREASURY USER; do
	load_key "$role"
done
distinct=$(printf '%s\n' "$DEPLOYER" "$ADMIN" "$OPERATOR" "$TREASURY" "$USER" | tr '[:upper:]' '[:lower:]' | sort -u | wc -l)
[ "$distinct" -eq 5 ] || die "the five keys of the keys file must be different"

started_at=$(utc_now)
started_ms=$(now_ms)
echo "Network: Base Sepolia, chain ID $BASE_SEPOLIA_CHAIN_ID; RPC: $(provider_name "$RPC"); start: $started_at"
echo "Accounts and ETH balances:"
for role in DEPLOYER ADMIN OPERATOR TREASURY USER; do
	balance=$(ETH_RPC_URL="$RPC" cast balance --ether "${!role}" 2>/dev/null) || die "cannot read the balance of $role"
	printf '  %-8s %s  %s ETH\n' "$role" "${!role}" "$balance"
done
[ "$(ETH_RPC_URL="$RPC" cast balance "$DEPLOYER" 2>/dev/null)" != 0 ] || die "DEPLOYER has no ETH: fund it first"

forge_dir="$STATE_DIR/cas-sepolia-forge"
mkdir -p "$forge_dir"
chmod 700 "$forge_dir"
log="$forge_dir/deploy.log"

echo "Deploying MockUSDC and CardSpendController (forge script, broadcast by DEPLOYER)..."
set +e
(
	cd "$REPO_ROOT/contracts" || exit 1
	unset TOKEN
	export FOUNDRY_ETH_RPC_URL="$RPC" FOUNDRY_BROADCAST="$forge_dir/broadcast" FOUNDRY_CACHE_PATH="$forge_dir/cache"
	export DEPLOYER ADMIN OPERATOR TREASURY
	exec forge script script/Deploy.s.sol --broadcast --slow --private-key "$DEPLOYER_KEY" --sender "$DEPLOYER"
) 2>&1 | redact >"$log"
rc=${PIPESTATUS[0]}
set -e
if [ "$rc" -ne 0 ]; then
	tail -n 20 "$log" >&2
	die "forge script failed (exit $rc); the log without secrets: cas-sepolia-forge/deploy.log"
fi

run="$forge_dir/broadcast/Deploy.s.sol/$BASE_SEPOLIA_CHAIN_ID/run-latest.json"
[ -f "$run" ] || die "forge wrote no broadcast record"
created() {
	jq -r --arg name "$1" '[.transactions[] | select(.transactionType == "CREATE" and .contractName == $name)][0]
		| "\(.contractAddress) \(.hash)"' "$run"
}
receipt() {
	jq -r --arg hash "$1" '[.receipts[] | select(.transactionHash == $hash)][0] | "\(.blockNumber) \(.gasUsed) \(.status)"' "$run"
}
read -r TOKEN token_tx <<<"$(created MockUSDC)"
read -r CONTROLLER controller_tx <<<"$(created CardSpendController)"
[[ "$TOKEN" =~ ^0x[0-9a-fA-F]{40}$ && "$CONTROLLER" =~ ^0x[0-9a-fA-F]{40}$ ]] || die "no contract addresses in the broadcast record"
TOKEN=$(cast to-check-sum-address "$TOKEN")
CONTROLLER=$(cast to-check-sum-address "$CONTROLLER")
read -r token_block token_gas token_status <<<"$(receipt "$token_tx")"
read -r controller_block controller_gas controller_status <<<"$(receipt "$controller_tx")"
[ "$token_status" = 0x1 ] && [ "$controller_status" = 0x1 ] || die "a deployment transaction did not succeed"

same_address "$(chain_call "$RPC" "$CONTROLLER" 'token()(address)')" "$TOKEN" || die "token() of the controller is not MockUSDC"
same_address "$(chain_call "$RPC" "$CONTROLLER" 'treasury()(address)')" "$TREASURY" || die "treasury() of the controller is not TREASURY"
[ "$(chain_call "$RPC" "$TOKEN" 'decimals()(uint8)')" = "$TOKEN_DECIMALS" ] || die "decimals() of MockUSDC is not $TOKEN_DECIMALS"
admin_role=0x0000000000000000000000000000000000000000000000000000000000000000
operator_role=$(cast keccak OPERATOR_ROLE)
[ "$(chain_call "$RPC" "$CONTROLLER" 'hasRole(bytes32,address)(bool)' "$admin_role" "$ADMIN")" = true ] || die "ADMIN has no admin role"
[ "$(chain_call "$RPC" "$CONTROLLER" 'hasRole(bytes32,address)(bool)' "$operator_role" "$OPERATOR")" = true ] || die "OPERATOR has no OPERATOR_ROLE"

tmp="$DEPLOYMENT_FILE.tmp"
cat >"$tmp" <<EOF
# CAS deployment on Base Sepolia, written by scripts/sepolia/deploy.sh. No secret.
CHAIN_ID=$BASE_SEPOLIA_CHAIN_ID
REHEARSAL=${CAS_SEPOLIA_REHEARSAL:-0}
DEPLOYED_AT=$started_at
DEPLOYER=$DEPLOYER
ADMIN=$ADMIN
OPERATOR=$OPERATOR
TREASURY=$TREASURY
USER=$USER
TOKEN=$TOKEN
TOKEN_TX=$token_tx
TOKEN_BLOCK=$((token_block))
CONTROLLER=$CONTROLLER
CONTROLLER_TX=$controller_tx
CONTROLLER_BLOCK=$((controller_block))
EOF
mv "$tmp" "$DEPLOYMENT_FILE"

echo "Deployed in $(( ($(now_ms) - started_ms) / 1000 )) s:"
printf '  MockUSDC            %s  %s\n' "$TOKEN" "$(address_link "$TOKEN")"
printf '    tx %s block %d gas %d  %s\n' "$token_tx" "$((token_block))" "$((token_gas))" "$(tx_link "$token_tx")"
printf '  CardSpendController %s  %s\n' "$CONTROLLER" "$(address_link "$CONTROLLER")"
printf '    tx %s block %d gas %d  %s\n' "$controller_tx" "$((controller_block))" "$((controller_gas))" "$(tx_link "$controller_tx")"
echo "Checked: token() = MockUSDC, treasury() = TREASURY, decimals() = $TOKEN_DECIMALS, ADMIN has DEFAULT_ADMIN_ROLE, OPERATOR has OPERATOR_ROLE."
echo "Written: cas-sepolia-deployment.env next to the keys file. Next: setup.sh."
