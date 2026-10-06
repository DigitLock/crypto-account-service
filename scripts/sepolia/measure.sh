#!/usr/bin/env bash
# measure.sh: N sequential USD authorizations of one card through casctl sim against a running card-auth, for the p95
# measurement and the signals by source (docs/deployment-guide.md step 6; test plan S2-T806, S2-T807).
#
# What it does:
#   1. checks eth_chainId of CARD_AUTH_RPC_URL of the environment file (84532 only), then the keys file;
#   2. reads /metrics of card-auth;
#   3. sends N authorizations in sequence, each with its own auth_id, and records for each the client-side time
#      (casctl process start to its exit) and the decision;
#   4. reads /metrics again and prints a summary: N, approvals, declines by reason, p50/p95/max client-side, the p95
#      of auth_decision_seconds from the histogram (the buckets of this run, linear within a bucket), inclusion
#      signals of this run by source, start time in UTC, the RPC provider by name.
# Exits 1 when the p95 of auth_decision_seconds is above 2 s; the summary is printed first.
# Needs: go, curl, jq, cast, perl; register.sh done (cas-sepolia-credentials.env next to the keys file); card-auth
#   running (run-card-auth.sh). The processor password reaches casctl in its environment only.
# Variables: CAS_SEPOLIA_KEYS (required); CAS_SEPOLIA_REHEARSAL=1 for a rehearsal on a local Anvil with chain ID
#   84532; CAS_ENV_FILE (default .env of the repository); CAS_BIN_DIR (default bin/); CAS_MEASURE_N (default 30);
#   CAS_MEASURE_AMOUNT in USD (default 1.00); CAS_CARD_REF (default card_A); CAS_CARD_AUTH_URL (default
#   http://127.0.0.1:CARD_AUTH_HTTP_PORT); CAS_CARD_AUTH_HEALTH_URL (default http://127.0.0.1:CARD_AUTH_HEALTH_PORT).

. "$(dirname "$0")/lib.sh"

need go curl jq
primary=$(env_value CARD_AUTH_RPC_URL)
[ -n "$primary" ] || die "CARD_AUTH_RPC_URL is not set in the environment file"
start_checks CARD_AUTH_RPC_URL "$primary"
ws=$(env_value CARD_AUTH_RPC_WS_URL)

[ -f "$CREDENTIALS_FILE" ] || die "no cas-sepolia-credentials.env next to the keys file: run register.sh first"
username=$(file_value "$CREDENTIALS_FILE" CASCTL_PROCESSOR_USERNAME)
password=$(file_value "$CREDENTIALS_FILE" CASCTL_PROCESSOR_PASSWORD)
[ -n "$username" ] && [ -n "$password" ] || die "the processor pair is missing in cas-sepolia-credentials.env"
add_redact "$password"

n=${CAS_MEASURE_N:-30}
[[ "$n" =~ ^[1-9][0-9]{0,3}$ ]] || die "CAS_MEASURE_N must be an integer from 1 to 9999"
amount=${CAS_MEASURE_AMOUNT:-1.00}
[[ "$amount" =~ ^[0-9]{1,14}(\.[0-9]{1,4})?$ ]] || die "CAS_MEASURE_AMOUNT must be a decimal with at most 4 decimals"
card_ref=${CAS_CARD_REF:-card_A}
http_port=$(env_value CARD_AUTH_HTTP_PORT)
health_port=$(env_value CARD_AUTH_HEALTH_PORT)
api_url=${CAS_CARD_AUTH_URL:-http://127.0.0.1:${http_port:-8092}}
health_url=${CAS_CARD_AUTH_HEALTH_URL:-http://127.0.0.1:${health_port:-8093}}
if rehearsal; then
	check_url_place CAS_CARD_AUTH_URL "$api_url"
	check_url_place CAS_CARD_AUTH_HEALTH_URL "$health_url"
fi

casctl=$(go_build casctl)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl -fsS "$health_url/metrics" >"$work/before" 2>/dev/null || die "cannot read /metrics of card-auth: is it running?"

started_at=$(utc_now)
prefix="m-$(date -u +%Y%m%dT%H%M%SZ)"
if [ -n "$ws" ]; then listener="on, $(provider_name "$ws")"; else listener=off; fi
echo "Measurement: $n USD authorizations of $amount, card $card_ref, in sequence; start $started_at"
echo "  RPC $(provider_name "$primary"); listener $listener"
: >"$work/times"
: >"$work/decisions"
i=1
while [ "$i" -le "$n" ]; do
	auth_id="$prefix-$i"
	t0=$(now_ms)
	set +e
	out=$(CASCTL_CARD_AUTH_URL="$api_url" CASCTL_PROCESSOR_USERNAME="$username" CASCTL_PROCESSOR_PASSWORD="$password" \
		"$casctl" sim authorize --auth-id "$auth_id" --card-ref "$card_ref" --amount "$amount" --currency USD 2>"$work/err")
	rc=$?
	set -e
	t1=$(now_ms)
	ms=$((t1 - t0))
	if [ "$rc" -eq 0 ]; then
		decision=$(printf '%s' "$out" | jq -r '.body.decision // "HTTP_\(.status)"')
		reason=$(printf '%s' "$out" | jq -r '.body.decline_reason // ""')
	else
		decision=ERROR
		reason="casctl exit $rc: $(redact <"$work/err" | tr '\n' ' ' | cut -c1-120)"
	fi
	printf '  %4d %-24s %-8s %6d ms %s\n' "$i" "$auth_id" "$decision" "$ms" "$reason"
	echo "$ms" >>"$work/times"
	echo "$decision $reason" >>"$work/decisions"
	i=$((i + 1))
done
curl -fsS "$health_url/metrics" >"$work/after" 2>/dev/null || die "cannot read /metrics of card-auth after the run"

approvals=$(grep -c '^APPROVED' "$work/decisions" || true)
client=$(sort -n "$work/times" | awk '{ v[NR] = $1 } END {
	p50 = int((NR * 50 + 99) / 100); p95 = int((NR * 95 + 99) / 100);
	printf "p50 %d ms, p95 %d ms, max %d ms", v[p50], v[p95], v[NR] }')

# Deltas of this run: auth_decision_seconds buckets and inclusion_signals_total by source.
histogram=$(awk '
	FNR == 1 { file++ }
	/^auth_decision_seconds_bucket\{/ { match($0, /le="[^"]*"/); le = substr($0, RSTART + 4, RLENGTH - 5)
		if (file == 1) before[le] = $NF; else { after[le] = $NF; order[++k] = le } }
	END {
		total = after["+Inf"] - before["+Inf"]
		if (total <= 0) { print "none"; exit }
		rank = 0.95 * total; prev = 0; lower = 0
		for (j = 1; j <= k; j++) {
			le = order[j]; c = after[le] - before[le]
			if (c >= rank) {
				if (le == "+Inf") { printf "%.3f", lower; exit }
				if (c == prev) { printf "%.3f", le; exit }
				printf "%.3f", lower + (le - lower) * (rank - prev) / (c - prev); exit
			}
			prev = c; lower = le
		}
	}' "$work/before" "$work/after")
histogram=${histogram:-none}
signals=$(awk '
	FNR == 1 { file++ }
	/^inclusion_signals_total\{/ { match($0, /source="[^"]*"/); s = substr($0, RSTART + 8, RLENGTH - 9)
		if (file == 1) before[s] = $NF; else after[s] = $NF }
	END { for (s in after) printf "%s %d, ", s, after[s] - before[s] }' "$work/before" "$work/after" | sed 's/, $//')
declines=$(grep -v '^APPROVED' "$work/decisions" | sort | uniq -c | awk '{ $1 = $1 " ×"; print }' | paste -sd ';' - || true)

echo "Summary"
echo "  start (UTC)               $started_at"
echo "  RPC provider              $(provider_name "$primary"); listener $listener"
echo "  N                         $n"
echo "  approvals                 $approvals"
echo "  declines                  ${declines:-none}"
echo "  client-side               $client"
echo "  auth_decision_seconds p95 ${histogram} s (histogram of this run)"
echo "  inclusion signals         ${signals:-none}"
if [ "$approvals" -lt "$n" ]; then
	echo "WARNING: $((n - approvals)) of $n authorizations were not approved"
fi
if [ "$histogram" = none ] || awk -v p="$histogram" 'BEGIN { exit !(p > 2) }'; then
	echo "FAIL: the p95 of auth_decision_seconds is above 2 s or was not recorded"
	exit 1
fi
echo "PASS: the p95 of auth_decision_seconds is at most 2 s"
