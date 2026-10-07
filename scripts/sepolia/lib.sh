# shellcheck shell=bash
# Shared rules of the Base Sepolia scripts (docs/deployment-guide.md). Sourced by every script, never run.
#
# - Keys come only from the file named by CAS_SEPOLIA_KEYS. The file must exist, be a regular file outside the
#   repository working tree, and give no permission to group or others (chmod 600).
# - Every script first checks eth_chainId of its RPC and refuses anything but 84532.
# - CAS_SEPOLIA_REHEARSAL=1 is the rehearsal switch: every URL must then be local (loopback). Without it a local
#   URL is refused, so a local Anvil can never pass for Base Sepolia.
# - A key or a URL is never printed. forge and cast take a private key only as an argument (--private-key): it is
#   passed inside the script process only, never typed on a command line of the shell.
# - The RPC of deploy.sh, setup.sh and register.sh: CAS_SEPOLIA_RPC_URL, else CARD_AUTH_RPC_URL of the environment
#   file, else https://sepolia.base.org (select_rpc).
# - The RPC of deploy.sh, setup.sh and register.sh: CAS_SEPOLIA_RPC_URL, else CARD_AUTH_RPC_URL of the environment
#   file, else https://sepolia.base.org (select_rpc). The URL is never printed.
# - Files written next to the keys file: cas-sepolia-deployment.env (addresses and hashes, no secret),
#   cas-sepolia-credentials.env (processor pair and service token, mode 600), cas-sepolia-forge/ (forge output).
# Needs bash 3.2 or later and perl (path and mode checks, timing, redaction).

set -euo pipefail

readonly BASE_SEPOLIA_CHAIN_ID=84532
readonly PUBLIC_RPC_URL=https://sepolia.base.org
readonly EXPLORER_URL=https://sepolia.basescan.org
readonly TOKEN_DECIMALS=6

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)
readonly REPO_ROOT
SCRIPT_NAME=$(basename "$0")
readonly SCRIPT_NAME

# Secrets loaded by this process, one per line; redact removes them from any output of forge, cast or casctl.
REDACT_LIST=""

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

rehearsal() { [ "${CAS_SEPOLIA_REHEARSAL:-}" = 1 ]; }

add_redact() { REDACT_LIST="${REDACT_LIST}${1}"$'\n'; }

# redact copies stdin to stdout without the loaded secrets and without any user:password@ of a URL.
redact() {
	CAS_REDACT="$REDACT_LIST" perl -pe '
		BEGIN { @s = sort { length($b) <=> length($a) } grep { length } split /\n/, ($ENV{CAS_REDACT} // "") }
		for my $s (@s) { s/\Q$s\E/[redacted]/gi }
		s{://[^/\s:@]+:[^/\s@]*@}{://[redacted]@}g;'
}

# url_host prints the host of a URL in lower case, without user info, port and path.
url_host() {
	local rest=${1#*://}
	rest=${rest%%/*}
	rest=${rest%%\?*}
	rest=${rest##*@}
	case "$rest" in
	\[*) rest=${rest%%]*}] ;;
	*) rest=${rest%%:*} ;;
	esac
	printf '%s' "$rest" | tr '[:upper:]' '[:lower:]'
}

is_local_host() {
	case "$1" in
	localhost | 127.* | '[::1]') return 0 ;;
	*) return 1 ;;
	esac
}

# provider_name names the RPC provider of a URL without any part of the URL.
provider_name() {
	local host
	host=$(url_host "$1")
	case "$host" in
	*alchemy.com) printf 'Alchemy' ;;
	sepolia.base.org) printf 'Base public endpoint' ;;
	*) if is_local_host "$host"; then printf 'local Anvil (rehearsal)'; else printf 'other provider'; fi ;;
	esac
}

# check_url_place NAME URL applies the rehearsal switch: local URLs only with it, no local URL without it.
check_url_place() {
	local name=$1 url=$2
	case "$url" in
	http://* | https://* | ws://* | wss://*) ;;
	*) die "$name is not an http, https, ws or wss URL" ;;
	esac
	if rehearsal; then
		is_local_host "$(url_host "$url")" || die "$name is not a local URL: CAS_SEPOLIA_REHEARSAL=1 allows local URLs only"
	else
		if is_local_host "$(url_host "$url")"; then
			die "$name is a local URL: only a rehearsal (CAS_SEPOLIA_REHEARSAL=1) runs on a local chain"
		fi
	fi
}

# check_chain NAME URL refuses an RPC whose eth_chainId is not 84532. The URL goes to cast through the
# environment, not the command line, and never appears in a message.
check_chain() {
	local name=$1 url=$2 id
	check_url_place "$name" "$url"
	add_redact "$url"
	id=$(ETH_RPC_URL="$url" cast chain-id 2>/dev/null) || die "eth_chainId of $name failed"
	[ "$id" = "$BASE_SEPOLIA_CHAIN_ID" ] || die "$name answers chain ID $id: only $BASE_SEPOLIA_CHAIN_ID (Base Sepolia) is allowed"
}

# check_keys_file applies the rules of the keys file and sets KEYS_FILE (absolute, symbolic links resolved)
# and STATE_DIR (its directory).
check_keys_file() {
	local mode
	[ -n "${CAS_SEPOLIA_KEYS:-}" ] || die "CAS_SEPOLIA_KEYS is not set: the path of the keys file, outside the repository"
	[ -e "$CAS_SEPOLIA_KEYS" ] || die "the keys file of CAS_SEPOLIA_KEYS does not exist"
	[ -f "$CAS_SEPOLIA_KEYS" ] || die "the keys file of CAS_SEPOLIA_KEYS is not a regular file"
	KEYS_FILE=$(perl -MCwd -e 'print Cwd::abs_path($ARGV[0])' -- "$CAS_SEPOLIA_KEYS")
	[ -n "$KEYS_FILE" ] || die "cannot resolve the path of CAS_SEPOLIA_KEYS"
	case "$KEYS_FILE" in
	"$REPO_ROOT"/*) die "the keys file of CAS_SEPOLIA_KEYS is inside the repository working tree: move it outside" ;;
	esac
	mode=$(perl -e 'printf "%04o", (stat $ARGV[0])[2] & 07777' -- "$KEYS_FILE")
	if [ $((8#$mode & 8#077)) -ne 0 ]; then
		die "the keys file of CAS_SEPOLIA_KEYS has mode $mode: group and others must have no access (chmod 600)"
	fi
	STATE_DIR=$(dirname "$KEYS_FILE")
	DEPLOYMENT_FILE="$STATE_DIR/cas-sepolia-deployment.env"
	CREDENTIALS_FILE="$STATE_DIR/cas-sepolia-credentials.env"
}

# file_value FILE NAME prints the value of the line NAME=value of FILE; an empty output when there is none.
# The file is read as data, never sourced.
file_value() {
	local file=$1 name=$2 line value=""
	while IFS= read -r line || [ -n "$line" ]; do
		line=${line%$'\r'}
		case "$line" in
		"$name="*) value=${line#*=} ;;
		esac
	done <"$file"
	printf '%s' "$value"
}

# read_key NAME prints the key NAME of the keys file as 0x and 64 hexadecimal characters.
read_key() {
	local value
	value=$(file_value "$KEYS_FILE" "$1")
	[ -n "$value" ] || die "$1 is missing in the keys file"
	value=${value#0x}
	[[ "$value" =~ ^[0-9a-fA-F]{64}$ ]] || die "$1 in the keys file is not 32 bytes as 64 hexadecimal characters"
	printf '0x%s' "$value"
}

# key_address KEY prints the address of a key. cast takes the key only as an argument.
key_address() {
	cast wallet address --private-key "$1" 2>/dev/null || die "cannot derive an address from a key of the keys file"
}

# load_key ROLE reads ROLE_PRIVATE_KEY, registers it for redaction and sets <ROLE>_KEY and <ROLE> (the address).
load_key() {
	local role=$1 key addr
	key=$(read_key "${role}_PRIVATE_KEY")
	add_redact "$key"
	add_redact "${key#0x}"
	addr=$(key_address "$key")
	printf -v "${role}_KEY" '%s' "$key"
	printf -v "$role" '%s' "$addr"
}

# deployment NAME prints a value of the deployment file written by deploy.sh.
deployment() {
	[ -f "$DEPLOYMENT_FILE" ] || die "no deployment file next to the keys file: run deploy.sh first"
	local value
	value=$(file_value "$DEPLOYMENT_FILE" "$1")
	[ -n "$value" ] || die "$1 is missing in the deployment file"
	printf '%s' "$value"
}

# env_file prints the path of the environment file of the repository: CAS_ENV_FILE or .env.
env_file() {
	local file=${CAS_ENV_FILE:-$REPO_ROOT/.env}
	[ -f "$file" ] || die "the environment file is missing: .env of the repository, or CAS_ENV_FILE"
	printf '%s' "$file"
}

# env_value NAME prints the value of NAME in the environment file, as make reads it: NAME=value, no quotes removed.
env_value() {
	local file line key value=""
	file=$(env_file)
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
	done <"$file"
	printf '%s' "$value"
}

# chain_call URL TO SIGNATURE ARGS... prints the first word of the decoded result of an eth_call; cast appends a
# second form to large numbers.
chain_call() {
	local url=$1 out
	shift
	out=$(ETH_RPC_URL="$url" cast call "$@" 2>/dev/null) || die "eth_call $2 failed"
	printf '%s' "${out%% *}"
}

# same_address A B is true when A and B are the same address, whatever the letter case.
same_address() { [ "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')" ]; }

# select_rpc sets RPC, the URL of forge and cast, and RPC_NAME, where it comes from (S2 st9b f): CAS_SEPOLIA_RPC_URL;
# else CARD_AUTH_RPC_URL of the environment file, when the file sets it; else the public endpoint. The public endpoint
# is load-balanced: a nonce read right after a transaction may be stale (Deployment Guide §10).
select_rpc() {
	local file=${CAS_ENV_FILE:-$REPO_ROOT/.env} value=""
	if [ -n "${CAS_SEPOLIA_RPC_URL:-}" ]; then
		RPC=$CAS_SEPOLIA_RPC_URL
		RPC_NAME=CAS_SEPOLIA_RPC_URL
		return
	fi
	if [ -f "$file" ]; then
		value=$(env_value CARD_AUTH_RPC_URL)
	fi
	if [ -n "$value" ]; then
		RPC=$value
		RPC_NAME=CARD_AUTH_RPC_URL
	else
		RPC=$PUBLIC_RPC_URL
		RPC_NAME="the public endpoint"
	fi
}

# tx_link and address_link print an explorer link, or a note in a rehearsal.
tx_link() {
	if rehearsal; then printf 'local tx %s' "$1"; else printf '%s/tx/%s' "$EXPLORER_URL" "$1"; fi
}
address_link() {
	if rehearsal; then printf 'local %s' "$1"; else printf '%s/address/%s' "$EXPLORER_URL" "$1"; fi
}

# now_ms prints the wall clock in milliseconds.
now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time() * 1000'; }

utc_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# base_units AMOUNT prints a decimal token amount in base units of the funding token (6 decimals).
base_units() {
	[[ "$1" =~ ^[0-9]{1,12}(\.[0-9]{1,6})?$ ]] || die "amount $1 is not a decimal with at most 12 integer digits and 6 decimals"
	local int=${1%%.*} frac=""
	case "$1" in *.*) frac=${1#*.} ;; esac
	while [ ${#frac} -lt "$TOKEN_DECIMALS" ]; do frac="${frac}0"; done
	printf '%s' "$(( 10#$int * 1000000 + 10#$frac ))"
}

# usdc BASE_UNITS prints base units as a decimal amount with 6 decimals.
usdc() { printf '%d.%06d' "$(( $1 / 1000000 ))" "$(( $1 % 1000000 ))"; }

# go_build TOOL builds ./cmd/TOOL into CAS_BIN_DIR (default bin/ of the repository) and prints the binary path.
go_build() {
	local dir=${CAS_BIN_DIR:-$REPO_ROOT/bin}
	mkdir -p "$dir"
	(cd "$REPO_ROOT" && go build -o "$dir/$1" "./cmd/$1") >&2 || die "go build ./cmd/$1 failed"
	printf '%s' "$dir/$1"
}

# start_checks NAME URL applies the rules every script starts with: tools, then the chain ID of its RPC, then the
# keys file.
start_checks() {
	need perl cast
	export FOUNDRY_OFFLINE=true NO_COLOR=1
	check_chain "$1" "$2"
	check_keys_file
}
