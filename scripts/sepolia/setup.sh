#!/usr/bin/env bash
# setup.sh: prepares the deployed pair for the card tests on Base Sepolia (docs/deployment-guide.md step 2;
# test plan S2-T805, S2-T806). Idempotent where the chain allows it: a step whose target already holds sends nothing.
#
# What it does, in order:
#   1. USER mints MockUSDC to itself up to CAS_SETUP_MINT (only the missing part; mint is open to anyone);
#   2. USER sets its allowance to the controller to CAS_SETUP_ALLOWANCE, when it differs;
#   3. ADMIN sets USER's wallet daily limit to CAS_SETUP_DAILY_LIMIT. The contract has no view of the limit, so
#      this step is sent on every run; the same value again changes nothing;
#   4. TREASURY sets its allowance to the controller for refunds to CAS_SETUP_REFUND_ALLOWANCE, when it differs.
# Prints every transaction hash with its explorer link and the resulting values.
# Needs: cast, perl; deploy.sh done (cas-sepolia-deployment.env next to the keys file); ETH of USER, ADMIN and
#   TREASURY for gas.
# Variables: CAS_SEPOLIA_KEYS (required); CAS_SEPOLIA_RPC_URL (default https://sepolia.base.org);
#   CAS_SEPOLIA_REHEARSAL=1 for a rehearsal on a local Anvil with chain ID 84532. Amounts in USDC, decimals allowed:
#   CAS_SETUP_MINT (default 100), CAS_SETUP_ALLOWANCE (100), CAS_SETUP_DAILY_LIMIT (50),
#   CAS_SETUP_REFUND_ALLOWANCE (100). The defaults cover T805 (5 USD) and T806 (30 × 1 USD) in one UTC day: 35 of
#   the 50 USDC of the daily limit.

. "$(dirname "$0")/lib.sh"

need jq
RPC=$(rpc_url)
start_checks CAS_SEPOLIA_RPC_URL "$RPC"

TOKEN=$(deployment TOKEN)
CONTROLLER=$(deployment CONTROLLER)
for role in USER ADMIN TREASURY; do
	load_key "$role"
	recorded=$(deployment "$role")
	same_address "${!role}" "$recorded" || die "the key $role of the keys file is not the $role of the deployment"
done

mint=$(base_units "${CAS_SETUP_MINT:-100}")
allowance=$(base_units "${CAS_SETUP_ALLOWANCE:-100}")
daily_limit=$(base_units "${CAS_SETUP_DAILY_LIMIT:-50}")
refund_allowance=$(base_units "${CAS_SETUP_REFUND_ALLOWANCE:-100}")


# send ROLE STEP TO SIGNATURE ARGS... sends one transaction as ROLE and prints its hash and link.
send() {
	local role=$1 step=$2 key_var="${1}_KEY" out hash status block gas
	shift 2
	set +e
	out=$(ETH_RPC_URL="$RPC" cast send --json --private-key "${!key_var}" "$@" 2>&1)
	local rc=$?
	set -e
	if [ "$rc" -ne 0 ]; then
		printf '%s\n' "$out" | redact | tail -n 5 >&2
		die "$step: the transaction of $role failed"
	fi
	hash=$(printf '%s' "$out" | jq -r .transactionHash)
	status=$(printf '%s' "$out" | jq -r .status)
	block=$(printf '%s' "$out" | jq -r .blockNumber)
	gas=$(printf '%s' "$out" | jq -r .gasUsed)
	[ "$status" = 0x1 ] || die "$step: transaction $hash reverted"
	printf '  %s: tx %s block %d gas %d  %s\n' "$step" "$hash" "$((block))" "$((gas))" "$(tx_link "$hash")"
}

echo "Setup on Base Sepolia, RPC $(provider_name "$RPC"), start $(utc_now)"
echo "  MockUSDC $TOKEN, controller $CONTROLLER, USER $USER, TREASURY $TREASURY"

balance=$(chain_call "$RPC" "$TOKEN" 'balanceOf(address)(uint256)' "$USER")
if [ "$balance" -lt "$mint" ]; then
	send USER "1. mint $(usdc $((mint - balance))) USDC to USER" "$TOKEN" 'mint(address,uint256)' "$USER" "$((mint - balance))"
else
	echo "  1. mint: USER holds $(usdc "$balance") USDC, at least $(usdc "$mint"): nothing sent"
fi

current=$(chain_call "$RPC" "$TOKEN" 'allowance(address,address)(uint256)' "$USER" "$CONTROLLER")
if [ "$current" != "$allowance" ]; then
	send USER "2. USER approves $(usdc "$allowance") USDC to the controller" "$TOKEN" 'approve(address,uint256)' "$CONTROLLER" "$allowance"
else
	echo "  2. approve: the allowance of USER is $(usdc "$allowance") USDC: nothing sent"
fi

send ADMIN "3. ADMIN sets the daily limit of USER to $(usdc "$daily_limit") USDC" "$CONTROLLER" 'setDailyLimit(address,uint256)' "$USER" "$daily_limit"

current=$(chain_call "$RPC" "$TOKEN" 'allowance(address,address)(uint256)' "$TREASURY" "$CONTROLLER")
if [ "$current" != "$refund_allowance" ]; then
	send TREASURY "4. TREASURY approves $(usdc "$refund_allowance") USDC to the controller for refunds" "$TOKEN" 'approve(address,uint256)' "$CONTROLLER" "$refund_allowance"
else
	echo "  4. refund allowance: the allowance of TREASURY is $(usdc "$refund_allowance") USDC: nothing sent"
fi

echo "Result:"
echo "  USER balance               $(usdc "$(chain_call "$RPC" "$TOKEN" 'balanceOf(address)(uint256)' "$USER")") USDC"
echo "  USER allowance             $(usdc "$(chain_call "$RPC" "$TOKEN" 'allowance(address,address)(uint256)' "$USER" "$CONTROLLER")") USDC"
echo "  USER remaining daily limit $(usdc "$(chain_call "$RPC" "$CONTROLLER" 'remainingDailyLimit(address)(uint256)' "$USER")") USDC"
echo "  TREASURY refund allowance  $(usdc "$(chain_call "$RPC" "$TOKEN" 'allowance(address,address)(uint256)' "$TREASURY" "$CONTROLLER")") USDC"
echo "  TREASURY balance           $(usdc "$(chain_call "$RPC" "$TOKEN" 'balanceOf(address)(uint256)' "$TREASURY")") USDC"
echo "Next: register.sh."
