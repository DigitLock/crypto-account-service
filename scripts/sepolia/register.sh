#!/usr/bin/env bash
# register.sh: registers the tenant, its credentials, the wallet connection of USER on base-sepolia and the card
# card_A through the public paths of CAS (docs/deployment-guide.md step 3; SRS — Core UC-105, CreateConnection,
# RegisterCard). No SQL.
#
# What it does, in order; a rerun continues from the credentials file and repeats nothing that is done:
#   1. casctl tenant create CAS_TENANT;
#   2. casctl processor issue: the Basic pair of the processor API of card-auth;
#   3. casctl token issue: the service token of the gRPC API of server;
#   4. ConnectionService/CreateConnection: owner CAS_OWNER_REF, source base-sepolia, wallet USER of the deployment;
#      ALREADY_EXISTS reuses the connection found by ListConnections;
#   5. CardService/RegisterCard: CAS_CARD_REF on that connection, daily limit CAS_CARD_DAILY_LIMIT.
# The processor password and the service token go only to cas-sepolia-credentials.env next to the keys file (mode
# 600), never to the output. The token reaches buf curl on its standard input, not on its command line.
# Needs: go, buf, jq, cast, perl; deploy.sh done; server running (make run); CASCTL_DATABASE_URL (owner role) in the
#   environment file. buf curl takes the schema from the frozen image proto/frozen/cas_v1.json: no reflection needed.
# Variables: CAS_SEPOLIA_KEYS (required); CAS_SEPOLIA_RPC_URL (default https://sepolia.base.org);
#   CAS_SEPOLIA_REHEARSAL=1 for a rehearsal on a local Anvil with chain ID 84532; CAS_ENV_FILE (default .env of the
#   repository); CAS_BIN_DIR (default bin/); CAS_GRPC_ADDR (default 127.0.0.1:50053); CAS_TENANT (default
#   sepolia-demo); CAS_OWNER_REF (default owner-a); CAS_CARD_REF (default card_A); CAS_CARD_DAILY_LIMIT in USDC
#   (default 200).

. "$(dirname "$0")/lib.sh"

need go buf jq
RPC=$(rpc_url)
start_checks CAS_SEPOLIA_RPC_URL "$RPC"

USER_ADDRESS=$(deployment USER)
tenant=${CAS_TENANT:-sepolia-demo}
owner_ref=${CAS_OWNER_REF:-owner-a}
card_ref=${CAS_CARD_REF:-card_A}
card_limit=$(base_units "${CAS_CARD_DAILY_LIMIT:-200}")
grpc_addr=${CAS_GRPC_ADDR:-127.0.0.1:50053}
if rehearsal; then
	is_local_host "$(url_host "http://$grpc_addr")" || die "CAS_GRPC_ADDR is not local: CAS_SEPOLIA_REHEARSAL=1 allows local addresses only"
fi
schema="$REPO_ROOT/proto/frozen/cas_v1.json"

casctl_db=$(env_value CASCTL_DATABASE_URL)
[ -n "$casctl_db" ] || die "CASCTL_DATABASE_URL is not set in the environment file"
add_redact "$casctl_db"
casctl=$(go_build casctl)

# casctl_run ARGS... runs casctl with the owner role connection string in its environment only.
casctl_run() { CASCTL_DATABASE_URL="$casctl_db" "$casctl" "$@"; }

# credential NAME prints a value of the credentials file; empty when it is not there.
credential() {
	if [ -f "$CREDENTIALS_FILE" ]; then file_value "$CREDENTIALS_FILE" "$1"; fi
}

# remember NAME VALUE appends a line to the credentials file, which only the owner can read.
remember() {
	(
		umask 077
		printf '%s=%s\n' "$1" "$2" >>"$CREDENTIALS_FILE"
	)
	chmod 600 "$CREDENTIALS_FILE"
}

echo "Registration on server $grpc_addr for Base Sepolia, start $(utc_now)"

if [ -f "$CREDENTIALS_FILE" ]; then
	recorded=$(credential CAS_TENANT)
	[ "$recorded" = "$tenant" ] || die "cas-sepolia-credentials.env belongs to another tenant: set CAS_TENANT to it, or move the file away"
fi

if [ -z "$(credential CAS_TENANT)" ]; then
	out=$(casctl_run tenant create "$tenant" 2>&1) || die "casctl tenant create failed: $(printf '%s' "$out" | redact)"
	echo "  1. $out"
	remember CAS_TENANT "$tenant"
else
	echo "  1. tenant $tenant: created before"
fi

if [ -z "$(credential CASCTL_PROCESSOR_PASSWORD)" ]; then
	pair=$(casctl_run processor issue "$tenant" 2>/dev/null) || die "casctl processor issue failed"
	username=${pair%%:*}
	password=${pair#*:}
	[[ "$username" =~ ^[0-9a-f]{12}$ && "$password" =~ ^[0-9a-f]{64}$ ]] || die "casctl processor issue printed no pair"
	add_redact "$password"
	remember CASCTL_PROCESSOR_USERNAME "$username"
	remember CASCTL_PROCESSOR_PASSWORD "$password"
	echo "  2. processor credential $username issued; the password is in cas-sepolia-credentials.env"
else
	echo "  2. processor credential $(credential CASCTL_PROCESSOR_USERNAME): issued before"
fi

if [ -z "$(credential CAS_SERVICE_TOKEN)" ]; then
	token=$(casctl_run token issue "$tenant" 2>/dev/null) || die "casctl token issue failed"
	[[ "$token" =~ ^cas_[0-9a-f]{12}_[0-9a-f]{64}$ ]] || die "casctl token issue printed no token"
	remember CAS_SERVICE_TOKEN "$token"
	echo "  3. service token ${token:4:12} issued; the token is in cas-sepolia-credentials.env"
else
	token=$(credential CAS_SERVICE_TOKEN)
	echo "  3. service token ${token:4:12}: issued before"
fi
add_redact "$token"

# grpc METHOD JSON calls a method of server; the response goes to stdout, the error JSON of buf curl to stderr.
grpc() {
	printf 'Authorization: Bearer %s\n\n%s\n' "$token" "$2" |
		buf curl --protocol grpc --http2-prior-knowledge --schema "$schema" -H @- -d @- "http://$grpc_addr/$1"
}

errors=$(mktemp)
trap 'rm -f "$errors"' EXIT

request=$(jq -cn --arg o "$owner_ref" --arg w "$USER_ADDRESS" \
	'{owner_ref: $o, source: "base-sepolia", label: "Card wallet", wallet: {address: $w}}')
if response=$(grpc cas.v1.ConnectionService/CreateConnection "$request" 2>"$errors"); then
	connection_id=$(printf '%s' "$response" | jq -r '.connection.connectionId')
	echo "  4. wallet connection $connection_id created: base-sepolia, $USER_ADDRESS"
elif [ "$(jq -r '.code // empty' "$errors" 2>/dev/null)" = already_exists ]; then
	list=$(grpc cas.v1.ConnectionService/ListConnections "$(jq -cn --arg o "$owner_ref" '{owner_ref: $o, page_size: 500}')" 2>"$errors") ||
		die "ListConnections failed: $(redact <"$errors")"
	connection_id=$(printf '%s' "$list" | jq -r --arg w "$USER_ADDRESS" \
		'[.connections[]? | select(.source == "base-sepolia" and ((.walletAddress // "") | ascii_downcase) == ($w | ascii_downcase))][0].connectionId // empty')
	[ -n "$connection_id" ] || die "the wallet is connected in the tenant, but not for owner $owner_ref"
	echo "  4. wallet connection $connection_id: created before"
else
	die "CreateConnection failed: $(redact <"$errors")"
fi

request=$(jq -cn --arg c "$card_ref" --arg o "$owner_ref" --arg id "$connection_id" --arg l "$card_limit" \
	'{card_ref: $c, owner_ref: $o, connection_id: $id, daily_limit: $l}')
response=$(grpc cas.v1.CardService/RegisterCard "$request" 2>"$errors") || die "RegisterCard failed: $(redact <"$errors")"
printf '%s' "$response" | jq -r '.card | "  5. card \(.cardRef): \(.status), wallet \(.walletAddress), daily limit \(.dailyLimit) base units"'

echo "Written: cas-sepolia-credentials.env next to the keys file (mode 600). Next: run-card-auth.sh."
