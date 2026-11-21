#!/usr/bin/env bash
# start.sh — 启动番茄阅读器（本机版，无需任何外部签名服务）
#
# 用法：
#   ./start.sh                 # 编译并启动（默认 8080）
#   ./start.sh --stop          # 停止
#   ./start.sh --status        # 查看状态
#
# 环境变量：
#   FQ_PORT          监听端口（默认 8080）
#   FQ_FONT_MAP      字体映射表路径（默认 data/font_map.json）
#   PREFETCH_WINDOW  预取前后章数（默认 3）
#   PREFETCH_DELAY   预取每章间隔毫秒（默认 1500）

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

GO_PORT="${FQ_PORT:-8080}"
RUN_DIR="$ROOT/.run"
mkdir -p "$RUN_DIR"

GO_PID="$RUN_DIR/go.pid"
GO_LOG="$RUN_DIR/go.log"

C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_END=$'\033[0m'
info() { printf '%s\n' "${C_DIM}[$(date +%H:%M:%S)]${C_END} $*"; }
ok()   { printf '%s\n' "${C_OK}✓${C_END} $*"; }
warn() { printf '%s\n' "${C_WARN}!${C_END} $*"; }
err()  { printf '%s\n' "${C_ERR}✗${C_END} $*"; }

health() { curl -fsS --noproxy '*' "http://127.0.0.1:${GO_PORT}/api/health" >/dev/null 2>&1; }

stop_all() {
    if [[ -f "$GO_PID" ]]; then
        pid="$(cat "$GO_PID")"
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null
            for _ in {1..20}; do kill -0 "$pid" 2>/dev/null || break; sleep 0.2; done
        fi
        rm -f "$GO_PID"
    fi
    ok "已停止"
}

case "${1:-}" in
    --stop)   stop_all; exit 0 ;;
    --status)
        if health; then ok "运行中：http://localhost:${GO_PORT}"; else warn "未运行"; fi
        exit 0 ;;
    "") ;;
    *) err "未知参数：$1（可用：--stop / --status）"; exit 1 ;;
esac

stop_all >/dev/null 2>&1 || true

info "编译服务…"
if ! go build -o "$ROOT/server" ./cmd/server; then
    err "编译失败"
    exit 1
fi

info "启动服务…"
nohup "$ROOT/server" \
    -addr ":${GO_PORT}" \
    -font-map "${FQ_FONT_MAP:-data/font_map.json}" \
    -prefetch-window "${PREFETCH_WINDOW:-3}" \
    -prefetch-delay-ms "${PREFETCH_DELAY:-1500}" \
    >> "$GO_LOG" 2>&1 &
echo $! > "$GO_PID"

for _ in {1..50}; do
    if health; then
        ok "服务就绪：http://localhost:${GO_PORT}"
        ok "日志：$GO_LOG"
        exit 0
    fi
    sleep 0.2
done

err "启动超时，最近日志："
tail -20 "$GO_LOG"
exit 1
