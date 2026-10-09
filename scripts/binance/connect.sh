#!/usr/bin/env bash
# connect.sh: connects the owner's Binance account to CAS through the public paths, and deletes the connection
# (X1 D-19, X1 D-39; test plan X1-T506, X1-T602, X1-T608). Run by the owner only, in st7, against a local server.
#
# Usage:
#   scripts/binance/connect.sh create
#     1. casctl tenant create x1-real, when the tenant does not exist;
#     2. casctl token issue x1-real into the credentials file, when it holds no token of x1-real;
#     3. ConnectionService/CreateConnection: owner_ref owner-x1, label Binance, source binance, exchange_key from
#        BINANCE_API_KEY and BINANCE_API_SECRET of the environment file.
#     Prints connection_id, status, key fingerprint and permissions: what CreateConnection returns. ip_restricted is
#     in the audit row of CONNECTION_CREATED only (X1 D-2); this script does not read it.
#   scripts/binance/connect.sh delete <connection_id>
#     ConnectionService/DeleteConnection with the token of the credentials file.
#
# Rules: the key and the secret are read from the environment file as data, never sourced; the bash builtin printf
# writes them, one per line, to the standard input of jq, which builds the request (X1 D-40); the token, the header and
# the body reach buf curl on its standard input (-H @- -d @-). No external command gets a secret in its argv or its
# environment. No secret on a command line, in the shell history, in a file left behind or in the output. On an error
# the gRPC code, message and reason are printed, never the request; the reason only when it is a code of capitals
# (X1 D-41).
# Needs: bash, go, buf, jq; server running (make run); CASCTL_DATABASE_URL (owner role) in the environment file.
#   buf curl takes the schema from the frozen image proto/frozen/cas_v1.json.
# Variables: CAS_ENV_FILE (default .env of the repository); CAS_X1_CREDENTIALS (default ~/.config/cas/x1-real.env,
#   outside the repository, mode 600); CAS_GRPC_ADDR (default 127.0.0.1:50053, local only); CAS_BIN_DIR (default bin/).

set -euo pipefail

readonly TENANT=x1-real OWNER_REF=owner-x1 LABEL=Binance SOURCE=binance
SCRIPT_NAME=$(basename "$0")
readonly SCRIPT_NAME
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)
readonly REPO_ROOT

die() {
	printf '%s: %s\n' "$SCRIPT_NAME" "$*" >&2
	exit 1
}

need() {
	local tool
	for tool in "$@"; do
		command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH"
	done
}

# is_local_addr HOST:PORT is true for a loopback host.
is_local_addr() {
	local host=${1%:*}
	case "$host" in
	localhost | 127.* | '[::1]') return 0 ;;
	*) return 1 ;;
	esac
}

# env_value NAME prints the value of NAME in the environment file, as make reads it. The file is read as data.
env_value() {
	local line key value=""
	while IFS= read -r line || [ -n "$line" ]; do
		line=${line%$'\r'}
		case "$line" in
		'' | '#'*) continue ;;
		esac
		key=${line%%=*}
		key=${key//[[:space:]]/}
		if [ "$key" = "$1" ]; then
			value=${line#*=}
			value=${value#"${value%%[![:space:]]*}"}
		fi
	done <"$ENV_FILE"
	printf '%s' "$value"
}

# file_value FILE NAME prints the value of the line NAME=value of FILE.
file_value() {
	local line value=""
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
		"$2="*) value=${line#*=} ;;
		esac
	done <"$1"
	printf '%s' "$value"
}

usage() { die "usage: $SCRIPT_NAME create | $SCRIPT_NAME delete <connection_id>"; }

[ $# -ge 1 ] || usage
command=$1
case "$command" in
create) [ $# -eq 1 ] || usage ;;
delete)
	[ $# -eq 2 ] || usage
	[[ "$2" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || die "the connection_id is not a UUID"
	;;
*) usage ;;
esac

need go buf jq
grpc_addr=${CAS_GRPC_ADDR:-127.0.0.1:50053}
is_local_addr "$grpc_addr" || die "CAS_GRPC_ADDR is not local: the script talks to a server on this machine only"
ENV_FILE=${CAS_ENV_FILE:-$REPO_ROOT/.env}
[ -f "$ENV_FILE" ] || die "the environment file is missing: .env of the repository, or CAS_ENV_FILE"
schema="$REPO_ROOT/proto/frozen/cas_v1.json"
[ -f "$schema" ] || die "the frozen schema proto/frozen/cas_v1.json is missing"

CREDENTIALS_FILE=${CAS_X1_CREDENTIALS:-$HOME/.config/cas/x1-real.env}
cred_dir=$(dirname "$CREDENTIALS_FILE")
case "$(cd "$cred_dir" 2>/dev/null && pwd -P || printf '%s' "$cred_dir")/" in
"$REPO_ROOT"/*) die "the credentials file is inside the repository working tree: set CAS_X1_CREDENTIALS outside it" ;;
esac

token=""
if [ -f "$CREDENTIALS_FILE" ]; then
	mode=$(ls -l "$CREDENTIALS_FILE" | cut -c5-10)
	[ "$mode" = "------" ] || die "the credentials file gives access to group or others: chmod 600 it"
	[ "$(file_value "$CREDENTIALS_FILE" CAS_TENANT)" = "$TENANT" ] ||
		die "the credentials file belongs to another tenant: set CAS_X1_CREDENTIALS to another path"
	token=$(file_value "$CREDENTIALS_FILE" CAS_SERVICE_TOKEN)
fi

errors=$(mktemp "${TMPDIR:-/tmp}/cas-connect.XXXXXX")
trap 'rm -f "$errors"' EXIT

# grpc METHOD calls a method of server with the body of its standard input; the response goes to stdout, the error JSON
# of buf curl to the errors file.
grpc() {
	{
		printf 'Authorization: Bearer %s\n\n' "$token"
		cat
	} | buf curl --protocol grpc --http2-prior-knowledge --schema "$schema" -H @- -d @- "http://$grpc_addr/$1" 2>"$errors"
}

# failed WHAT prints the gRPC code, message and reason of the errors file and exits.
failed() {
	local code message reason
	code=$(jq -r '.code // empty' "$errors" 2>/dev/null || true)
	if [ -z "$code" ]; then
		die "$1 failed: $(head -c 300 "$errors")"
	fi
	message=$(jq -r '.message // empty' "$errors")
	# The reason of google.rpc.ErrorInfo: decoded by buf when its schema knows the type, else read from the encoded
	# message, whose first field is the reason in capitals (X1 D-41). Printed only when it is such a code.
	reason=$(jq -r '[.details[]? | select(.type == "google.rpc.ErrorInfo") |
		(.debug.reason? // ((.value // "") | @base64d | [match("[A-Z][A-Z_]+").string][0]) // empty)][0] // empty' "$errors" 2>/dev/null || true)
	[[ "$reason" =~ ^[A-Z][A-Z_]+$ ]] || reason=""
	die "$1 failed: $code: $message${reason:+ ($reason)}"
}

case "$command" in
create)
	key=$(env_value BINANCE_API_KEY)
	secret=$(env_value BINANCE_API_SECRET)
	[ -n "$key" ] && [ -n "$secret" ] || die "BINANCE_API_KEY and BINANCE_API_SECRET must be set in the environment file"
	casctl_db=$(env_value CASCTL_DATABASE_URL)
	[ -n "$casctl_db" ] || die "CASCTL_DATABASE_URL is not set in the environment file"
	bin_dir=${CAS_BIN_DIR:-$REPO_ROOT/bin}
	mkdir -p "$bin_dir"
	(cd "$REPO_ROOT" && go build -o "$bin_dir/casctl" ./cmd/casctl) >&2 || die "go build ./cmd/casctl failed"
	casctl_run() { CASCTL_DATABASE_URL="$casctl_db" "$bin_dir/casctl" "$@"; }

	if casctl_run tenant list 2>/dev/null | awk -v t="$TENANT" 'NR > 1 && $1 == t { found = 1 } END { exit !found }'; then
		echo "Tenant $TENANT: exists"
	else
		casctl_run tenant create "$TENANT" >/dev/null 2>&1 || die "casctl tenant create $TENANT failed"
		echo "Tenant $TENANT: created"
	fi

	if [ -z "$token" ]; then
		token=$(casctl_run token issue "$TENANT" 2>/dev/null) || die "casctl token issue failed"
		[[ "$token" =~ ^cas_[0-9a-f]{12}_[0-9a-f]{64}$ ]] || die "casctl token issue printed no token"
		(
			umask 077
			mkdir -p "$cred_dir"
			printf 'CAS_TENANT=%s\nCAS_SERVICE_TOKEN=%s\n' "$TENANT" "$token" >"$CREDENTIALS_FILE"
		)
		chmod 600 "$CREDENTIALS_FILE"
		echo "Service token ${token:4:12}: issued into the credentials file (mode 600)"
	else
		echo "Service token ${token:4:12}: from the credentials file"
	fi

	# The constants of the request are in the filter; the key and the secret reach jq on its standard input, written by
	# the builtin printf: in no argv and no environment of an external command (X1 D-40).
	response=$(printf '%s\n%s\n' "$key" "$secret" | jq -cnR \
		"[inputs] as [\$k, \$s] | {owner_ref: \"$OWNER_REF\", source: \"$SOURCE\", label: \"$LABEL\",
		  exchange_key: {api_key: \$k, api_secret: \$s}}" |
		grpc cas.v1.ConnectionService/CreateConnection) || failed CreateConnection
	printf '%s' "$response" | jq -r '.connection |
		"connection_id: \(.connectionId)\nstatus: \(.status)\nkey_fingerprint: \(.keyFingerprint)\npermissions: \(.permissions // [] | join(","))"'
	;;
delete)
	[ -n "$token" ] || die "no token of $TENANT in the credentials file: run create first"
	ID="$2" jq -cn '{connection_id: env.ID}' | grpc cas.v1.ConnectionService/DeleteConnection >/dev/null || failed DeleteConnection
	echo "connection_id: $2"
	echo "deleted"
	;;
esac
