#!/usr/bin/env bash
# End-to-end harness for the llm-tracing CPA plugin.
#
# Usage: ./run.sh [path/to/llm-tracing.dll]
#   With a DLL: loads the plugin, sends scenario requests, captures OTLP traces and verifies them.
#   Without:    "harness-only" mode; plugin disabled, trace assertions skipped.
#
# Env overrides: GO (go binary), CPA_BIN (cli-proxy-api.exe), RECEIVER_FAIL_FIRST (inject 503s),
#                TRACE_TIMEOUT_S (default 15), KEEP_RUNNING=1 (do not kill processes; debugging only).
set -uo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_DIR="$E2E_DIR/run"
GO="${GO:-/c/Program Files/Go/bin/go.exe}"
CPA_BIN="${CPA_BIN:-$E2E_DIR/../../CLIProxyAPI/cli-proxy-api.exe}"
PLUGIN_DLL="${1:-}"
TRACE_TIMEOUT_S="${TRACE_TIMEOUT_S:-15}"
RECEIVER_FAIL_FIRST="${RECEIVER_FAIL_FIRST:-0}"

CPA_URL="http://127.0.0.1:8318"
MOCK_URL="http://127.0.0.1:18080"
OTLP_URL="http://127.0.0.1:4318"
CLIENT_KEY="e2e-client-key-0001"
MGMT_KEY="e2e-mgmt-secret"

winpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else echo "$1"; fi; }
log() { printf '[run.sh %s] %s\n' "$(date +%H:%M:%S)" "$*"; }

PIDS=()
cleanup() {
	local rc=$?
	if [[ "${KEEP_RUNNING:-0}" == "1" ]]; then
		log "KEEP_RUNNING=1: leaving processes running: ${PIDS[*]:-}"
		exit $rc
	fi
	for pid in "${PIDS[@]:-}"; do
		[[ -z "$pid" ]] && continue
		local winpid=""
		[[ -r "/proc/$pid/winpid" ]] && winpid="$(cat "/proc/$pid/winpid" 2>/dev/null)"
		if [[ -n "$winpid" ]] && command -v taskkill >/dev/null 2>&1; then
			taskkill //F //T //PID "$winpid" >/dev/null 2>&1 || true
		fi
		kill "$pid" >/dev/null 2>&1 || true
	done
	wait >/dev/null 2>&1 || true
	log "all harness processes stopped"
	exit $rc
}
trap cleanup EXIT INT TERM

fail() { log "ERROR: $*"; exit 1; }

port_busy() { curl -s -o /dev/null --max-time 1 "$1" ; }

# --- prepare run dir -------------------------------------------------------
[[ -x "$CPA_BIN" || -f "$CPA_BIN" ]] || fail "CPA binary not found: $CPA_BIN"
for u in "$CPA_URL" "$MOCK_URL" "$OTLP_URL"; do
	port_busy "$u" && fail "something is already listening on $u; stop it first"
done

rm -rf "$RUN_DIR"
mkdir -p "$RUN_DIR/bin" "$RUN_DIR/auths" "$RUN_DIR/plugins"

log "building harness programs"
for p in mockupstream otlpreceiver verify; do
	(cd "$E2E_DIR" && "$GO" build -o "$RUN_DIR/bin/$p.exe" "./cmd/$p") || fail "build $p failed"
done
VERIFY="$RUN_DIR/bin/verify.exe"

MODE="plugin"
PLUGINS_ENABLED="true"
if [[ -z "$PLUGIN_DLL" ]]; then
	MODE="harness-only"
	PLUGINS_ENABLED="false"
else
	[[ -f "$PLUGIN_DLL" ]] || fail "plugin DLL not found: $PLUGIN_DLL"
	# Loader scans <dir>/<goos>/<goarch>/ then <dir>/; plugin ID = file name without extension
	# (optionally "<id>-v<version>.dll"). The ID must match plugins.configs.llm-tracing.
	mkdir -p "$RUN_DIR/plugins/windows/amd64"
	cp "$PLUGIN_DLL" "$RUN_DIR/plugins/windows/amd64/llm-tracing.dll" || fail "copy plugin failed"
fi
log "mode: $MODE"

RUN_WIN="$(winpath "$RUN_DIR")"
sed -e "s#__E2E_RUN__#$RUN_WIN#g" -e "s#__PLUGINS_ENABLED__#$PLUGINS_ENABLED#g" \
	"$E2E_DIR/config.e2e.yaml" >"$RUN_DIR/config.yaml"

# --- start processes ---------------------------------------------------------
"$RUN_DIR/bin/mockupstream.exe" -addr 127.0.0.1:18080 >"$RUN_DIR/mock.log" 2>&1 &
PIDS+=($!)
"$RUN_DIR/bin/otlpreceiver.exe" -addr 127.0.0.1:4318 -out "$(winpath "$RUN_DIR/traces.jsonl")" \
	-fail-first "$RECEIVER_FAIL_FIRST" >"$RUN_DIR/receiver.log" 2>&1 &
PIDS+=($!)
(cd "$RUN_DIR" && exec "$CPA_BIN" --config "$(winpath "$RUN_DIR/config.yaml")" --local-model --no-browser) \
	>"$RUN_DIR/cpa.log" 2>&1 &
PIDS+=($!)

wait_for() { # name url timeout_s [curl args...]
	local name="$1" url="$2" timeout="$3"
	shift 3
	local deadline=$((SECONDS + timeout))
	while ((SECONDS < deadline)); do
		if curl -sf -o /dev/null --max-time 2 "$@" "$url"; then
			log "$name ready"
			return 0
		fi
		sleep 0.2
	done
	return 1
}
wait_for "mockupstream" "$MOCK_URL/_stats" 15 || fail "mockupstream not ready (see run/mock.log)"
wait_for "otlpreceiver" "$OTLP_URL/dump" 15 || fail "otlpreceiver not ready (see run/receiver.log)"
wait_for "CPA" "$CPA_URL/v1/models" 60 -H "Authorization: Bearer $CLIENT_KEY" ||
	{ tail -n 40 "$RUN_DIR/cpa.log"; fail "CPA not ready on 8318 (see run/cpa.log)"; }
curl -s -H "Authorization: Bearer $CLIENT_KEY" "$CPA_URL/v1/models" >"$RUN_DIR/models.json"
log "CPA models: $(tr -d '\n' <"$RUN_DIR/models.json" | grep -o '"id":"[^"]*"' | tr '\n' ' ')"
if [[ "$MODE" == "plugin" ]]; then
	grep -i "plugin" "$RUN_DIR/cpa.log" | grep -i "llm-tracing" | head -n 5
fi

# --- scenarios ---------------------------------------------------------------
: >"$RUN_DIR/scenarios.jsonl"
N=0
scenario() { # name kind model stream(true|false)
	local name="$1" kind="$2" model="$3" stream="$4"
	N=$((N + 1))
	local body_file="resp_${N}_${name}.txt" stats_file="stats_${N}_${name}.json"
	local payload
	if [[ "$stream" == "true" ]]; then
		payload="{\"model\":\"$model\",\"stream\":true,\"stream_options\":{\"include_usage\":true},\"messages\":[{\"role\":\"user\",\"content\":\"hello from e2e $name\"}]}"
	else
		payload="{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"hello from e2e $name\"}]}"
	fi
	local code
	code="$(curl -sN -o "$RUN_DIR/$body_file" -w '%{http_code}' --max-time 60 \
		-H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
		-d "$payload" "$CPA_URL/v1/chat/completions")"
	[[ -z "$code" ]] && code=0
	curl -s "$MOCK_URL/_stats" >"$RUN_DIR/$stats_file"
	printf '{"name":"%s","kind":"%s","model":"%s","stream":%s,"status":%d,"body_file":"%s","stats_file":"%s"}\n' \
		"$name" "$kind" "$model" "$stream" "$code" "$body_file" "$stats_file" >>"$RUN_DIR/scenarios.jsonl"
	log "scenario $N $name: HTTP $code"
}

# Baseline stats so the first scenario's upstream delta excludes startup traffic.
curl -s "$MOCK_URL/_stats" >"$RUN_DIR/stats_0_baseline.json"

scenario "nonstream-success" success mock-gpt false
scenario "stream-success" success mock-gpt true
scenario "retry-429-then-ok" retry mock-retry false
scenario "all-fail" fail mock-fail false
scenario "alias-success" success mock-alias false
scenario "stream-retry-429-then-ok" retry mock-retry true

curl -s "$MOCK_URL/_stats" >"$RUN_DIR/mock_stats_final.json"
curl -s -H "Authorization: Bearer $MGMT_KEY" "$CPA_URL/v8/management/observability/usage/queue?count=200" \
	>"$RUN_DIR/usage_queue.json"

# --- traces --------------------------------------------------------------------
VERIFY_ARGS=(-run "$(winpath "$RUN_DIR")")
if [[ "$MODE" == "plugin" ]]; then
	log "waiting up to ${TRACE_TIMEOUT_S}s for $N root spans"
	deadline=$((SECONDS + TRACE_TIMEOUT_S))
	roots=0
	while ((SECONDS < deadline)); do
		curl -s "$OTLP_URL/dump" >"$RUN_DIR/dump.json"
		roots="$("$VERIFY" -run "$(winpath "$RUN_DIR")" -count-roots 2>/dev/null || echo 0)"
		((roots >= N)) && break
		sleep 0.25
	done
	# Short grace period for late child spans after the last root arrives.
	sleep 1
	curl -s "$OTLP_URL/dump" >"$RUN_DIR/dump.json"
	log "root spans received: $roots / $N"
else
	curl -s "$OTLP_URL/dump" >"$RUN_DIR/dump.json"
	VERIFY_ARGS+=(-harness-only)
fi

"$VERIFY" "${VERIFY_ARGS[@]}"
RC=$?
log "artifacts in $RUN_DIR (cpa.log, mock.log, receiver.log, dump.json, usage_queue.json)"
exit $RC
