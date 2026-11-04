#!/usr/bin/env bash
# End-to-end harness for the llm-tracing CPA plugin.
#
# Usage: ./run.sh [path/to/llm-tracing.dll]
#   With a DLL: loads the plugin, sends scenario requests, captures OTLP traces and verifies them.
#               Then (unless API_CHECKS=0) queries the v0.2.0 Traces API on the same CPA, and
#               restarts CPA with endpoint "" to verify the API works without an OTLP backend.
#   Without:    "harness-only" mode; plugin disabled, trace and API assertions skipped.
#
# Env overrides: GO (go binary), CPA_BIN (cli-proxy-api.exe), RECEIVER_FAIL_FIRST (inject 503s),
#                TRACE_TIMEOUT_S (default 15), API_CHECKS (default 1; 0 for pre-0.2.0 DLLs),
#                KEEP_RUNNING=1 (do not kill processes; debugging only).
set -uo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_DIR="$E2E_DIR/run"
NOEXPORT_DIR="$RUN_DIR/noexport"
GO="${GO:-/c/Program Files/Go/bin/go.exe}"
CPA_BIN="${CPA_BIN:-$E2E_DIR/../../CLIProxyAPI/cli-proxy-api.exe}"
PLUGIN_DLL="${1:-}"
TRACE_TIMEOUT_S="${TRACE_TIMEOUT_S:-15}"
RECEIVER_FAIL_FIRST="${RECEIVER_FAIL_FIRST:-0}"
API_CHECKS="${API_CHECKS:-1}"

# 4318 is the standard OTLP/HTTP port and is often taken by a local Jaeger/collector,
# so the harness receiver uses 14318. 8317 (a user's own CPA) is never touched.
CPA_URL="http://127.0.0.1:8318"
MOCK_URL="http://127.0.0.1:18080"
OTLP_ADDR="127.0.0.1:14318"
OTLP_URL="http://$OTLP_ADDR"
CLIENT_KEY="e2e-client-key-0001"
MGMT_KEY="e2e-mgmt-key-0001" # plaintext of management.secret-key in config.e2e.yaml
API_PREFIX="/v0/management/plugins/llm-tracing"
API_V8_PREFIX="/v8/management/plugins/llm-tracing"
RES_PREFIX="/v0/resource/plugins/llm-tracing"

winpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else echo "$1"; fi; }
log() { printf '[run.sh %s] %s\n' "$(date +%H:%M:%S)" "$*"; }

kill_pid() {
	local pid="$1" winpid=""
	[[ -z "$pid" ]] && return
	[[ -r "/proc/$pid/winpid" ]] && winpid="$(cat "/proc/$pid/winpid" 2>/dev/null)"
	if [[ -n "$winpid" ]] && command -v taskkill >/dev/null 2>&1; then
		taskkill //F //T //PID "$winpid" >/dev/null 2>&1 || true
	fi
	kill "$pid" >/dev/null 2>&1 || true
	wait "$pid" >/dev/null 2>&1 || true
}

PIDS=()
cleanup() {
	local rc=$?
	if [[ "${KEEP_RUNNING:-0}" == "1" ]]; then
		log "KEEP_RUNNING=1: leaving processes running: ${PIDS[*]:-}"
		exit $rc
	fi
	for pid in "${PIDS[@]:-}"; do
		kill_pid "$pid"
	done
	wait >/dev/null 2>&1 || true
	log "all harness processes stopped"
	exit $rc
}
trap cleanup EXIT INT TERM

fail() { log "ERROR: $*"; exit 1; }

port_busy() { curl -s -o /dev/null --max-time 1 "$1"; }

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

# --- prepare run dir -------------------------------------------------------
[[ -x "$CPA_BIN" || -f "$CPA_BIN" ]] || fail "CPA binary not found: $CPA_BIN"
for u in "$CPA_URL" "$MOCK_URL" "$OTLP_URL"; do
	port_busy "$u" && fail "something is already listening on $u; stop it first"
done

rm -rf "$RUN_DIR"
mkdir -p "$RUN_DIR/bin"

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
fi
log "mode: $MODE (API_CHECKS=$API_CHECKS)"

# prepare_cpa_dir dir endpoint: renders config.yaml and installs the plugin under <dir>.
prepare_cpa_dir() {
	local dir="$1" endpoint="$2"
	mkdir -p "$dir/auths" "$dir/plugins"
	# One disabled OAuth-style auth file, so the plugin's host.auth.list lookup has an entry to
	# resolve (config API keys are not file-backed). Disabled: never refreshed or routed to.
	# Its e-mail must never appear in the OTLP dump or the API responses.
	printf '{"type":"claude","email":"e2e-oauth-fixture@example.com","disabled":true}\n' >"$dir/auths/claude-e2e-oauth-fixture.json"
	if [[ "$MODE" == "plugin" ]]; then
		# Loader scans <dir>/<goos>/<goarch>/ then <dir>/; plugin ID = file name without extension
		# (optionally "<id>-v<version>.dll"). The ID must match plugins.configs.llm-tracing.
		mkdir -p "$dir/plugins/windows/amd64"
		cp "$PLUGIN_DLL" "$dir/plugins/windows/amd64/llm-tracing.dll" || fail "copy plugin failed"
	fi
	sed -e "s#__E2E_RUN__#$(winpath "$dir")#g" -e "s#__PLUGINS_ENABLED__#$PLUGINS_ENABLED#g" \
		-e "s#__OTLP_ENDPOINT__#$endpoint#g" "$E2E_DIR/config.e2e.yaml" >"$dir/config.yaml"
}

CPA_PID=""
start_cpa() { # dir
	local dir="$1"
	(cd "$dir" && exec "$CPA_BIN" --config "$(winpath "$dir/config.yaml")" --local-model --no-browser) \
		>"$dir/cpa.log" 2>&1 &
	CPA_PID=$!
	PIDS+=("$CPA_PID")
	wait_for "CPA" "$CPA_URL/v1/models" 60 -H "Authorization: Bearer $CLIENT_KEY" ||
		{ tail -n 40 "$dir/cpa.log"; fail "CPA not ready on 8318 (see $dir/cpa.log)"; }
	curl -s -H "Authorization: Bearer $CLIENT_KEY" "$CPA_URL/v1/models" >"$dir/models.json"
	log "CPA models: $(tr -d '\n' <"$dir/models.json" | grep -o '"id":"[^"]*"' | tr '\n' ' ')"
	if [[ "$MODE" == "plugin" ]]; then
		grep -i "plugin" "$dir/cpa.log" | grep -i "llm-tracing" | head -n 5
	fi
}

stop_cpa() {
	kill_pid "$CPA_PID"
	local deadline=$((SECONDS + 15))
	while port_busy "$CPA_URL" && ((SECONDS < deadline)); do sleep 0.2; done
	port_busy "$CPA_URL" && fail "CPA did not stop"
	log "CPA stopped"
	CPA_PID=""
}

# --- scenarios ---------------------------------------------------------------
OUT="$RUN_DIR" # current output directory for scenario()/probe()
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
	code="$(curl -sN -o "$OUT/$body_file" -w '%{http_code}' --max-time 60 \
		-H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
		-d "$payload" "$CPA_URL/v1/chat/completions")"
	[[ -z "$code" ]] && code=0
	curl -s "$MOCK_URL/_stats" >"$OUT/$stats_file"
	printf '{"name":"%s","kind":"%s","model":"%s","stream":%s,"status":%d,"body_file":"%s","stats_file":"%s"}\n' \
		"$name" "$kind" "$model" "$stream" "$code" "$body_file" "$stats_file" >>"$OUT/scenarios.jsonl"
	log "scenario $N $name: HTTP $code"
}

begin_scenarios() { # dir
	OUT="$1"
	N=0
	: >"$OUT/scenarios.jsonl"
	# Baseline stats so the first scenario's upstream delta excludes earlier traffic.
	curl -s "$MOCK_URL/_stats" >"$OUT/stats_0_baseline.json"
}

# probe name auth(none|wrong|right) path [ext]: saves body to api_<name>.<ext>, headers to
# api_<name>.headers.txt and appends one line to api_probes.jsonl.
probe() {
	local name="$1" auth="$2" path="$3" ext="${4:-json}"
	local hdr=()
	case "$auth" in
	right) hdr=(-H "Authorization: Bearer $MGMT_KEY") ;;
	wrong) hdr=(-H "Authorization: Bearer wrong-mgmt-key-9999") ;;
	esac
	local body="api_$name.$ext" headers="api_$name.headers.txt" code
	code="$(curl -s -o "$OUT/$body" -D "$OUT/$headers" -w '%{http_code}' --max-time 15 "${hdr[@]}" "$CPA_URL$path")"
	[[ -z "$code" ]] && code=0
	printf '{"name":"%s","auth":"%s","path":"%s","status":%d,"body_file":"%s","headers_file":"%s"}\n' \
		"$name" "$auth" "$path" "$code" "$body" "$headers" >>"$OUT/api_probes.jsonl"
	log "probe $name ($auth) $path: HTTP $code"
}

# wait_api_traces n: polls /traces until it lists n traces (plugin completion grace).
wait_api_traces() {
	local want="$1" got=0 deadline=$((SECONDS + TRACE_TIMEOUT_S))
	while ((SECONDS < deadline)); do
		got="$(curl -s --max-time 5 -H "Authorization: Bearer $MGMT_KEY" "$CPA_URL$API_PREFIX/traces?limit=50" |
			grep -o '"trace_id"' | wc -l | tr -d ' ')"
		((got >= want)) && break
		sleep 0.25
	done
	log "API traces stored: $got / $want"
}

# --- phase 1: OTLP export ---------------------------------------------------------
prepare_cpa_dir "$RUN_DIR" "$OTLP_URL/v1/traces"

"$RUN_DIR/bin/mockupstream.exe" -addr 127.0.0.1:18080 >"$RUN_DIR/mock.log" 2>&1 &
PIDS+=($!)
"$RUN_DIR/bin/otlpreceiver.exe" -addr "$OTLP_ADDR" -out "$(winpath "$RUN_DIR/traces.jsonl")" \
	-fail-first "$RECEIVER_FAIL_FIRST" >"$RUN_DIR/receiver.log" 2>&1 &
PIDS+=($!)
wait_for "mockupstream" "$MOCK_URL/_stats" 15 || fail "mockupstream not ready (see run/mock.log)"
wait_for "otlpreceiver" "$OTLP_URL/dump" 15 || fail "otlpreceiver not ready (see run/receiver.log)"
start_cpa "$RUN_DIR"

begin_scenarios "$RUN_DIR"
scenario "nonstream-success" success mock-gpt false
scenario "stream-success" success mock-gpt true
scenario "retry-429-then-ok" retry mock-retry false
scenario "all-fail" fail mock-fail false
scenario "alias-success" success mock-alias false
scenario "stream-retry-429-then-ok" retry mock-retry true

curl -s "$MOCK_URL/_stats" >"$RUN_DIR/mock_stats_final.json"
curl -s -H "Authorization: Bearer $MGMT_KEY" "$CPA_URL/v8/management/observability/usage/queue?count=200" \
	>"$RUN_DIR/usage_queue.json"

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

if [[ "$MODE" != "plugin" || "$API_CHECKS" == "0" ]]; then
	log "Traces API checks skipped (mode=$MODE API_CHECKS=$API_CHECKS)"
	log "artifacts in $RUN_DIR (cpa.log, mock.log, receiver.log, dump.json, usage_queue.json)"
	exit $RC
fi

# --- phase 2: Traces API on the same CPA -------------------------------------------
# CPA bans an IP for 30 min after 5 consecutive bad management keys; every route's noauth/wrongkey
# probes are immediately followed by a valid-key probe, which resets the counter.
log "phase 2: Traces API"
OUT="$RUN_DIR"
: >"$RUN_DIR/api_probes.jsonl"
probe summary_noauth none "$API_PREFIX/summary"
probe summary_wrongkey wrong "$API_PREFIX/summary"
probe summary right "$API_PREFIX/summary"
probe traces_noauth none "$API_PREFIX/traces?limit=50"
probe traces_wrongkey wrong "$API_PREFIX/traces?limit=50"
probe traces right "$API_PREFIX/traces?limit=50"
RETRY_ID="$("$VERIFY" -run "$(winpath "$RUN_DIR")" -api-trace-id retry-429-then-ok 2>/dev/null)"
[[ -z "$RETRY_ID" ]] && RETRY_ID="00000000000000000000000000000000"
log "retry trace id: $RETRY_ID"
probe trace_retry_noauth none "$API_PREFIX/trace?id=$RETRY_ID"
probe trace_retry_wrongkey wrong "$API_PREFIX/trace?id=$RETRY_ID"
probe trace_retry right "$API_PREFIX/trace?id=$RETRY_ID"
probe trace_malformed right "$API_PREFIX/trace?id=not-a-hex-id"
probe trace_unknown right "$API_PREFIX/trace?id=0123456789abcdef0123456789abcdef"
probe traces_status_error right "$API_PREFIX/traces?status=error"
probe traces_status_ok right "$API_PREFIX/traces?status=ok"
probe traces_model_retry right "$API_PREFIX/traces?model=mock-retry"
probe traces_limit1 right "$API_PREFIX/traces?limit=1"
probe traces_minms_huge right "$API_PREFIX/traces?min_ms=600000"
probe traces_limit_negative right "$API_PREFIX/traces?limit=-5"
probe traces_limit_huge right "$API_PREFIX/traces?limit=99999"
probe traces_status_bogus right "$API_PREFIX/traces?status=bogus"
probe v8_summary right "$API_V8_PREFIX/summary"
probe res_dashboard none "$RES_PREFIX/dashboard" html
probe res_css none "$RES_PREFIX/dashboard.css" css
probe res_js none "$RES_PREFIX/dashboard.js" js

"$VERIFY" -run "$(winpath "$RUN_DIR")" -api
RC_API=$?

# --- phase 3: endpoint "" (UI without an OTLP backend) --------------------------------
log "phase 3: restart CPA with endpoint \"\""
stop_cpa
mkdir -p "$NOEXPORT_DIR"
curl -s "$OTLP_URL/dump" >"$NOEXPORT_DIR/receiver_before.json"
prepare_cpa_dir "$NOEXPORT_DIR" ""
start_cpa "$NOEXPORT_DIR"
begin_scenarios "$NOEXPORT_DIR"
scenario "nonstream-success" success mock-gpt false
scenario "all-fail" fail mock-fail false
wait_api_traces "$N"
# Longer than flush_interval_ms + completion_grace_ms: any export would have happened by now.
sleep 2
: >"$NOEXPORT_DIR/api_probes.jsonl"
probe summary right "$API_PREFIX/summary"
probe traces right "$API_PREFIX/traces?limit=50"
probe traces_status_error right "$API_PREFIX/traces?status=error"
curl -s "$OTLP_URL/dump" >"$NOEXPORT_DIR/receiver_after.json"

"$VERIFY" -run "$(winpath "$NOEXPORT_DIR")" -api -no-export \
	-receiver-before "$(winpath "$NOEXPORT_DIR/receiver_before.json")" \
	-receiver-after "$(winpath "$NOEXPORT_DIR/receiver_after.json")"
RC_NOEXPORT=$?

log "results: otlp=$RC api=$RC_API no-export=$RC_NOEXPORT"
log "artifacts in $RUN_DIR (cpa.log, dump.json, api_*.json, noexport/)"
((RC != 0 || RC_API != 0 || RC_NOEXPORT != 0)) && exit 1
exit 0
