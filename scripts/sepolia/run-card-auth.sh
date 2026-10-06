#!/usr/bin/env bash
# run-card-auth.sh: starts card-auth for Base Sepolia in the foreground (docs/deployment-guide.md step 4; test plan
# S2-T803, S2-T805). Stop it with Ctrl-C.
#
# What it does:
#   1. checks eth_chainId of CARD_AUTH_RPC_URL of the environment file and of the fallback (84532 only), then the
#      keys file;
#   2. checks that the OPERATOR key of the keys file is the OPERATOR of the deployment;
#   3. builds card-auth and starts it with a clean environment: the variables of card-auth from the environment file
#      (CARD_AUTH_*, CRS_ADDRESS, EVM_ALLOWED_CHAIN_IDS, LOG_LEVEL, LOG_FORMAT; OPERATOR_PRIVATE_KEY of the file is
#      ignored), then the values of Base Sepolia (SRS — Card Spend §3.1):
#        OPERATOR_PRIVATE_KEY from the keys file, in the process environment only;
#        CARD_AUTH_CHAIN_ID 84532; CARD_AUTH_CONTROLLER_ADDRESS and CARD_AUTH_TOKEN_ADDRESS of the deployment;
#        CARD_AUTH_TOKEN_DECIMALS 6; CARD_AUTH_FINALITY_MODE tag, CARD_AUTH_FINALITY_TAG finalized;
#        CARD_AUTH_LISTENER_SUBSCRIPTION pendingLogs; CARD_AUTH_RPC_FALLBACK_URL CAS_SEPOLIA_FALLBACK_URL.
#      CARD_AUTH_RPC_URL and CARD_AUTH_RPC_WS_URL stay as the environment file sets them. Without
#      CARD_AUTH_RPC_WS_URL there is no listener: decisions use receipt polling.
# Needs: go, cast, perl; deploy.sh done; CARD_AUTH_DATABASE_URL (role cas_card_auth) and CARD_AUTH_RPC_URL in the
#   environment file.
# Variables: CAS_SEPOLIA_KEYS (required); CAS_SEPOLIA_FALLBACK_URL (default https://sepolia.base.org);
#   CAS_SEPOLIA_REHEARSAL=1 for a rehearsal on a local Anvil with chain ID 84532; CAS_ENV_FILE (default .env of the
#   repository); CAS_BIN_DIR (default bin/).

. "$(dirname "$0")/lib.sh"

need go
primary=$(env_value CARD_AUTH_RPC_URL)
[ -n "$primary" ] || die "CARD_AUTH_RPC_URL is not set in the environment file"
start_checks CARD_AUTH_RPC_URL "$primary"
fallback=${CAS_SEPOLIA_FALLBACK_URL:-$PUBLIC_RPC_URL}
check_chain CAS_SEPOLIA_FALLBACK_URL "$fallback"
ws=$(env_value CARD_AUTH_RPC_WS_URL)
if [ -n "$ws" ]; then
	check_url_place CARD_AUTH_RPC_WS_URL "$ws"
	add_redact "$ws"
fi

load_key OPERATOR
same_address "$OPERATOR" "$(deployment OPERATOR)" || die "the key OPERATOR of the keys file is not the OPERATOR of the deployment"
controller=$(deployment CONTROLLER)
token=$(deployment TOKEN)

# The variables of card-auth in the environment file, as make reads them: NAME=value.
names=()
values=()
file=$(env_file)
while IFS= read -r line || [ -n "$line" ]; do
	line=${line%$'\r'}
	case "$line" in
	'' | '#'*) continue ;;
	esac
	name=${line%%=*}
	name=${name//[[:space:]]/}
	case "$name" in
	CARD_AUTH_* | CRS_ADDRESS | EVM_ALLOWED_CHAIN_IDS | LOG_LEVEL | LOG_FORMAT) ;;
	*) continue ;;
	esac
	value=${line#*=}
	names+=("$name")
	values+=("${value#"${value%%[![:space:]]*}"}")
done <"$file"

card_auth=$(go_build card-auth)

http_port=$(env_value CARD_AUTH_HTTP_PORT)
health_port=$(env_value CARD_AUTH_HEALTH_PORT)
echo "card-auth for Base Sepolia, start $(utc_now)"
echo "  chain ID $BASE_SEPOLIA_CHAIN_ID; controller $controller; token $token (decimals $TOKEN_DECIMALS); operator $OPERATOR"
echo "  RPC $(provider_name "$primary"); fallback $(provider_name "$fallback")"
if [ -n "$ws" ]; then
	echo "  listener: pendingLogs on CARD_AUTH_RPC_WS_URL ($(provider_name "$ws"))"
else
	echo "  listener: off, CARD_AUTH_RPC_WS_URL is not set: receipt polling only"
fi
echo "  finality: tag finalized; HTTP port ${http_port:-8092}, health port ${health_port:-8093}"

# A clean environment: nothing of the shell reaches card-auth but these variables.
for name in $(compgen -e); do
	case "$name" in
	PATH | HOME | TMPDIR | LANG | LC_ALL) ;;
	*) unset "$name" 2>/dev/null || true ;;
	esac
done
i=0
while [ "$i" -lt "${#names[@]}" ]; do
	export "${names[$i]}=${values[$i]}"
	i=$((i + 1))
done
export CARD_AUTH_CHAIN_ID="$BASE_SEPOLIA_CHAIN_ID"
export CARD_AUTH_CONTROLLER_ADDRESS="$controller" CARD_AUTH_TOKEN_ADDRESS="$token" CARD_AUTH_TOKEN_DECIMALS="$TOKEN_DECIMALS"
export CARD_AUTH_FINALITY_MODE=tag CARD_AUTH_FINALITY_TAG=finalized CARD_AUTH_LISTENER_SUBSCRIPTION=pendingLogs
export CARD_AUTH_RPC_FALLBACK_URL="$fallback"
export OPERATOR_PRIVATE_KEY="$OPERATOR_KEY"
exec "$card_auth"
