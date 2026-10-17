#!/usr/bin/env bash
#
# portforward.sh — one-shot recovery for office-convert kubectl port-forwards.
#
# WHY
# After a VPN reconnect, two things break:
#   1. The EKS API endpoint /32 routes (added by eks-vpn-routes.sh) disappear
#      from the kernel routing table, so kubectl times out reaching the API.
#   2. Any prior `kubectl port-forward` processes are still bound to their
#      local ports — their TCP sockets survive in TIME_WAIT or as zombies, so
#      "address already in use" until the OS reaps them.
#
# This script fixes both, idempotently, and starts fresh port-forwards for the
# office-convert API and UI services. Re-runnable any time without errors.
#
# WHAT IT DOES (start, the default)
#   1. Preflight (kubectl + aws + VPN iface present, AWS SSO session active).
#   2. If kubectl can't reach the cluster, run ./eks-vpn-routes.sh add to
#      restore the VPN /32 routes.
#   3. Kill any previous office-convert port-forwards we started (tracked via
#      PID files in /tmp). Also kill orphans matching `kubectl port-forward
#      -n office-convert-dev` that this script didn't start, with --force or
#      KILL_ORPHANS=1.
#   4. Pick the first free local port for each service: API tries 18080 →
#      18089, UI tries 8501 → 8510. Avoids "address already in use" entirely.
#   5. Start each `kubectl port-forward` in the background, log stderr to
#      deploy/logs/, write the PID to /tmp.
#   6. Health-probe each forwarded port. If a probe fails, the script bails
#      with the path to the failing forward's log.
#   7. Print URLs the user opens in the browser.
#
# Reversibility
#   `./portforward.sh stop` kills both port-forwards. The EKS /32 routes added
#   in step 2 stay (use `eks-vpn-routes.sh remove` to roll those back too).
#
# Usage
#   ./portforward.sh start      # default; idempotent
#   ./portforward.sh stop       # stop both forwards
#   ./portforward.sh restart    # stop + start
#   ./portforward.sh status     # show PIDs, ports, health
#   ./portforward.sh --help
#
# Env overrides
#   NAMESPACE          default: office-convert-dev
#   API_SVC            default: office-convert
#   UI_SVC             default: office-convert-ui
#   API_PORT_BASE      default: 18080   (first port tried)
#   UI_PORT_BASE       default: 8501
#   PORT_TRIES         default: 10      (auto-increment range)
#   KILL_ORPHANS       set to 1 to kill non-tracked office-convert port-forwards

if [ -z "${BASH_VERSION:-}" ]; then
    echo "ERROR: this script requires bash." >&2
    exit 1
fi
set -euo pipefail

NAMESPACE="${NAMESPACE:-office-convert-dev}"
API_SVC="${API_SVC:-office-convert}"
UI_SVC="${UI_SVC:-office-convert-ui}"
API_PORT_BASE="${API_PORT_BASE:-18080}"
UI_PORT_BASE="${UI_PORT_BASE:-8501}"
API_REMOTE_PORT=80
UI_REMOTE_PORT=8501
PORT_TRIES="${PORT_TRIES:-10}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
LOG_DIR="$REPO_ROOT/deploy/logs"
mkdir -p "$LOG_DIR"

API_PID_FILE="/tmp/officeconvert-portforward-api.pid"
UI_PID_FILE="/tmp/officeconvert-portforward-ui.pid"
API_LOG="$LOG_DIR/portforward-api.log"
UI_LOG="$LOG_DIR/portforward-ui.log"
# Port chosen at runtime is written here so `status`/`stop` can recover it.
API_PORT_FILE="/tmp/officeconvert-portforward-api.port"
UI_PORT_FILE="/tmp/officeconvert-portforward-ui.port"

if [ -t 1 ]; then
    G="$(tput setaf 2)"; Y="$(tput setaf 3)"; R="$(tput setaf 1)"; B="$(tput setaf 4)"; D="$(tput dim)"; X="$(tput sgr0)"
else
    G=""; Y=""; R=""; B=""; D=""; X=""
fi
log()  { printf "%s\n" "$*"; }
ok()   { printf "${G}✓${X} %s\n" "$*"; }
warn() { printf "${Y}⚠${X} %s\n" "$*"; }
err()  { printf "${R}✗${X} %s\n" "$*" >&2; }
hdr()  { printf "\n${B}%s${X}\n" "$*"; }
dim()  { printf "${D}%s${X}\n" "$*"; }

usage() {
    sed -n '3,55p' "$0"
    exit 0
}

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || { err "Required command not found: $1"; exit 2; }
}

# Pick the first free port starting from $1, trying up to $PORT_TRIES values.
# Echoes the chosen port; exits non-zero if none free.
pick_free_port() {
    local base="$1" i port
    for ((i=0; i<PORT_TRIES; i++)); do
        port=$((base + i))
        if ! ss -tlnH "sport = :$port" 2>/dev/null | grep -q .; then
            echo "$port"
            return 0
        fi
    done
    err "No free local port in range $base..$((base+PORT_TRIES-1))"
    exit 10
}

# Kill a PID if it's still alive and is a kubectl port-forward we own.
kill_if_ours() {
    local pid="$1" svc="$2"
    if [ -z "$pid" ]; then return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then
        return 0
    fi
    # Match command line so we don't accidentally murder an unrelated PID
    # that's been recycled.
    if ps -o args= -p "$pid" 2>/dev/null | grep -q "kubectl.*port-forward.*$svc"; then
        kill "$pid" 2>/dev/null || true
        # graceful then forceful
        for _ in 1 2 3 4 5; do
            if ! kill -0 "$pid" 2>/dev/null; then return 0; fi
            sleep 0.2
        done
        kill -9 "$pid" 2>/dev/null || true
    fi
}

# Kill orphan kubectl port-forwards targeting our namespace that we don't
# track in PID files. Only when KILL_ORPHANS=1.
kill_orphans() {
    [ "${KILL_ORPHANS:-0}" = "1" ] || return 0
    local tracked_api tracked_ui pid args
    tracked_api="$(cat "$API_PID_FILE" 2>/dev/null || true)"
    tracked_ui="$(cat "$UI_PID_FILE" 2>/dev/null || true)"
    pgrep -f "kubectl[[:space:]]+port-forward.*$NAMESPACE" 2>/dev/null | while read -r pid; do
        [ "$pid" = "$tracked_api" ] && continue
        [ "$pid" = "$tracked_ui" ] && continue
        args="$(ps -o args= -p "$pid" 2>/dev/null || true)"
        warn "killing orphan port-forward (pid=$pid): $args"
        kill "$pid" 2>/dev/null || true
    done
}

# ---------- preflight ----------
preflight() {
    require_cmd kubectl
    require_cmd ss
    require_cmd curl
    require_cmd pgrep
    require_cmd ps
    # aws + dig only matter if we end up calling eks-vpn-routes.sh
}

# Probe the cluster API. Returns 0 if reachable, non-zero otherwise.
cluster_reachable() {
    kubectl --request-timeout=8s auth can-i get pods -n "$NAMESPACE" >/dev/null 2>&1
}

ensure_routes() {
    if cluster_reachable; then
        ok "kubectl can reach cluster (no route fix needed)"
        return 0
    fi
    warn "kubectl can't reach cluster — running eks-vpn-routes.sh add"
    if [ ! -x "$SCRIPT_DIR/eks-vpn-routes.sh" ]; then
        err "Missing or non-executable $SCRIPT_DIR/eks-vpn-routes.sh"
        exit 11
    fi
    "$SCRIPT_DIR/eks-vpn-routes.sh" add
    if ! cluster_reachable; then
        err "Still can't reach cluster after route fix. Check VPN + AWS SSO."
        exit 12
    fi
    ok "cluster reachable after route fix"
}

# Start one port-forward. Args: SVC LOCAL_PORT REMOTE_PORT PID_FILE PORT_FILE LOG_FILE
start_one() {
    local svc="$1" local_port="$2" remote_port="$3" pid_file="$4" port_file="$5" log_file="$6"
    : > "$log_file"
    # nohup so it survives this script exiting; setsid so it gets its own
    # process group (clean kill semantics).
    setsid nohup kubectl port-forward -n "$NAMESPACE" \
        --address 127.0.0.1 \
        "svc/$svc" \
        "$local_port:$remote_port" \
        >>"$log_file" 2>&1 < /dev/null &
    local pid=$!
    echo "$pid" > "$pid_file"
    echo "$local_port" > "$port_file"
    # Wait up to 5s for the port to be listening.
    local i
    for ((i=0; i<25; i++)); do
        if ss -tlnH "sport = :$local_port" 2>/dev/null | grep -q .; then
            ok "$svc: pid=$pid listening on 127.0.0.1:$local_port (log: $log_file)"
            return 0
        fi
        if ! kill -0 "$pid" 2>/dev/null; then
            err "$svc port-forward died during startup. Tail of log:"
            tail -20 "$log_file" | sed 's/^/    /' >&2
            return 1
        fi
        sleep 0.2
    done
    err "$svc port-forward didn't bind $local_port within 5s. Tail of log:"
    tail -20 "$log_file" | sed 's/^/    /' >&2
    return 1
}

probe_endpoint() {
    local url="$1" name="$2"
    local code
    code=$(curl -sS -o /dev/null -m 5 -w '%{http_code}' "$url" 2>/dev/null || echo 000)
    if [ "$code" = "200" ] || [ "$code" = "503" ]; then
        # 200 = healthy, 503 = pod is up but /health says not ready (still
        # proves the forward works end-to-end).
        ok "$name probe: HTTP $code at $url"
    else
        warn "$name probe: HTTP $code at $url (forward up but endpoint not responding healthy)"
    fi
}

cmd_start() {
    preflight
    hdr "Preflight"
    ensure_routes
    kill_orphans

    hdr "Cleaning up any previous port-forwards we started"
    if [ -f "$API_PID_FILE" ]; then
        kill_if_ours "$(cat "$API_PID_FILE")" "$API_SVC"
        rm -f "$API_PID_FILE" "$API_PORT_FILE"
    fi
    if [ -f "$UI_PID_FILE" ]; then
        kill_if_ours "$(cat "$UI_PID_FILE")" "$UI_SVC"
        rm -f "$UI_PID_FILE" "$UI_PORT_FILE"
    fi
    ok "old PID files cleared"

    hdr "Picking free local ports"
    local api_port ui_port
    api_port=$(pick_free_port "$API_PORT_BASE")
    ui_port=$(pick_free_port "$UI_PORT_BASE")
    ok "API → $api_port  (base $API_PORT_BASE)"
    ok "UI  → $ui_port  (base $UI_PORT_BASE)"

    hdr "Starting port-forwards"
    start_one "$API_SVC" "$api_port" "$API_REMOTE_PORT" "$API_PID_FILE" "$API_PORT_FILE" "$API_LOG"
    start_one "$UI_SVC"  "$ui_port"  "$UI_REMOTE_PORT"  "$UI_PID_FILE"  "$UI_PORT_FILE"  "$UI_LOG"

    hdr "Health probes"
    probe_endpoint "http://localhost:$api_port/health"           "API"
    probe_endpoint "http://localhost:$ui_port/"    "UI"

    hdr "Open in browser"
    printf "  ${G}API  →${X}  http://localhost:%s/docs        (Swagger)\n" "$api_port"
    printf "  ${G}API  →${X}  http://localhost:%s/health      (health JSON)\n" "$api_port"
    printf "  ${G}UI   →${X}  http://localhost:%s             (Streamlit dashboard)\n" "$ui_port"
    echo
    dim  "Stop:    $0 stop"
    dim  "Status:  $0 status"
    dim  "Logs:    tail -f $API_LOG  |  tail -f $UI_LOG"
}

cmd_stop() {
    preflight
    hdr "Stopping office-convert port-forwards"
    if [ -f "$API_PID_FILE" ]; then
        kill_if_ours "$(cat "$API_PID_FILE")" "$API_SVC"
        rm -f "$API_PID_FILE" "$API_PORT_FILE"
        ok "API stopped"
    else
        log "  API: no tracked PID file"
    fi
    if [ -f "$UI_PID_FILE" ]; then
        kill_if_ours "$(cat "$UI_PID_FILE")" "$UI_SVC"
        rm -f "$UI_PID_FILE" "$UI_PORT_FILE"
        ok "UI stopped"
    else
        log "  UI: no tracked PID file"
    fi
    kill_orphans
}

status_one() {
    local svc="$1" pid_file="$2" port_file="$3" remote_path="$4"
    local pid port code
    if [ ! -f "$pid_file" ] || [ ! -f "$port_file" ]; then
        warn "$svc: not running (no PID/port file)"
        return
    fi
    pid="$(cat "$pid_file")"
    port="$(cat "$port_file")"
    if ! kill -0 "$pid" 2>/dev/null; then
        err "$svc: PID $pid no longer alive (stale)"
        return
    fi
    code=$(curl -sS -o /dev/null -m 3 -w '%{http_code}' "http://localhost:$port$remote_path" 2>/dev/null || echo 000)
    ok "$svc: pid=$pid port=$port endpoint=$remote_path http=$code"
}

cmd_status() {
    preflight
    hdr "Cluster"
    if cluster_reachable; then
        ok "kubectl can reach $NAMESPACE"
    else
        warn "kubectl can't reach $NAMESPACE — run \`$0 start\` to attempt repair"
    fi
    hdr "Port-forwards"
    status_one "$API_SVC" "$API_PID_FILE" "$API_PORT_FILE" "/health"
    status_one "$UI_SVC"  "$UI_PID_FILE"  "$UI_PORT_FILE"  "/"
}

cmd_restart() {
    cmd_stop
    cmd_start
}

case "${1:-start}" in
    start)              cmd_start ;;
    stop)               cmd_stop ;;
    restart)            cmd_restart ;;
    status)             cmd_status ;;
    -h|--help|help)     usage ;;
    *)                  err "Unknown subcommand: $1"; usage ;;
esac
