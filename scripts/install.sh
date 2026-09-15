#!/usr/bin/env bash
# starburst-backend 一键安装脚本
#
# 用法:
#   curl -fsSL https://<host>/install.sh | bash
# 或本地:
#   bash scripts/install.sh [--port 18880] [--db sqlite|postgres] [--pg-dsn "..."] [--admin-password "..."]
#
# 安装内容:
#   1. 下载 starburst-backend 单二进制到 /usr/local/bin
#   2. 写入 systemd 服务 (/etc/systemd/system/starburst-backend.service)
#   3. 生成默认配置 (/etc/starburst-backend/)
#   4. 启动并开机自启
#
#   通过环境变量/参数覆盖默认值:
#   OCB_PORT / --port
#   OCB_DB / --db (sqlite|postgres)
#   OCB_PG_DSN / --pg-dsn
#   OCB_ADMIN_PASSWORD / --admin-password
#   OCB_DEFAULT_TOKEN / --default-token (首次运行预置的 API token，客户端可免登录调用)
#   OCB_WORKERS / --workers (并发执行的任务数，默认 4)
#   OCB_TASK_RETENTION / --task-retention (保留已完成任务的时长，Go duration 语法，
#     例如 168h0m=7 天；不设置则永久保留)
#   OCB_PREFIX / --prefix
#     安装根目录，默认 /（即 /usr/local/bin、/etc/...、/var/lib/...）。
#     设成非根目录即"沙箱模式"：所有文件写到该目录下、跳过 systemctl，
#     用于非 root 或 CI 里验证脚本，不动真实系统。例:
#       OCB_BIN_URL=file:///tmp/ocb bash scripts/install.sh --prefix /tmp/sandbox
set -euo pipefail

# ---------- 参数解析 ----------
PORT="${OCB_PORT:-18880}"
DB="${OCB_DB:-sqlite}"
PG_DSN="${OCB_PG_DSN:-}"
ADMIN_PASSWORD="${OCB_ADMIN_PASSWORD:-}"
DEFAULT_TOKEN="${OCB_DEFAULT_TOKEN:-}"
WORKERS="${OCB_WORKERS:-4}"
TASK_RETENTION="${OCB_TASK_RETENTION:-}"
STT_URL="${OCB_STT_URL:-}"
STT_TIMEOUT="${OCB_STT_TIMEOUT:-}"
STT_MAX_CHUNK_BYTES="${OCB_STT_MAX_CHUNK_BYTES:-}"
PREFIX="${OCB_PREFIX:-/}"

while [[ $# -gt 0 ]]; do
  opt="$1"
  case "$opt" in
    -h|--help)
      echo "用法: $0 [--port 18880] [--db sqlite|postgres] [--pg-dsn dsn] [--admin-password pw] [--default-token tok] \\"
      echo "       [--workers 4] [--task-retention 168h0m] [--prefix /] \\"
      echo "       [--stt-url http://192.0.2.150:18090] [--stt-timeout 30s] [--stt-max-chunk-bytes 2097152]"
      exit 0 ;;
    --port|--db|--pg-dsn|--admin-password|--default-token|--workers|--task-retention|--prefix|--stt-url|--stt-timeout|--stt-max-chunk-bytes)
      if [[ $# -lt 2 ]]; then
        echo "!! 参数 $opt 需要一个值" >&2
        exit 1
      fi
      case "$opt" in
        --port) PORT="$2" ;;
        --db) DB="$2" ;;
        --pg-dsn) PG_DSN="$2" ;;
        --admin-password) ADMIN_PASSWORD="$2" ;;
        --default-token) DEFAULT_TOKEN="$2" ;;
        --workers) WORKERS="$2" ;;
        --task-retention) TASK_RETENTION="$2" ;;
        --prefix) PREFIX="$2" ;;
        --stt-url) STT_URL="$2" ;;
        --stt-timeout) STT_TIMEOUT="$2" ;;
        --stt-max-chunk-bytes) STT_MAX_CHUNK_BYTES="$2" ;;
      esac
      shift 2
      ;;
    *) echo "未知参数: $opt" >&2; exit 1 ;;
  esac
done

# 沙箱模式：前缀不是 / 时不碰真实系统目录，也不启动服务。
SYSTEMD=1
if [[ "$PREFIX" != "/" ]]; then
  PREFIX="${PREFIX%/}"
  SYSTEMD=0
fi

# ---------- 权限与前置检查 ----------
if [[ "$SYSTEMD" -eq 1 && "$(id -u)" -ne 0 ]]; then
  echo "!! 需要 root 权限（写入 /usr/local/bin、/etc/systemd/system 并执行 systemctl）" >&2
  echo "   请使用: sudo bash $0 $*" >&2
  exit 1
fi
if [[ "$SYSTEMD" -eq 1 ]]; then
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "!! 未找到 systemctl，本机可能不是 systemd 系统" >&2
    echo "   仅需验证脚本时可加 --prefix <目录> 跳过服务管理" >&2
    exit 1
  fi
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "!! 未找到 curl，请先安装 (如 apt install curl)" >&2
  exit 1
fi

# ---------- 参数合法性校验 ----------
if ! [[ "$PORT" =~ ^[0-9]+$ ]]; then
  echo "!! 端口必须是数字: $PORT" >&2
  exit 1
fi
if ! [[ "$WORKERS" =~ ^[0-9]+$ ]] || [[ "$WORKERS" -lt 1 ]]; then
  echo "!! --workers 必须是 >= 1 的整数: $WORKERS" >&2
  exit 1
fi
if [[ "$DB" != "sqlite" && "$DB" != "postgres" ]]; then
  echo "!! --db 仅支持 sqlite 或 postgres: $DB" >&2
  exit 1
fi
if [[ "$DB" == "postgres" && -z "$PG_DSN" ]]; then
  echo "!! 选择 postgres 时必须提供 --pg-dsn" >&2
  exit 1
fi
if [[ -n "$STT_URL" && ! "$STT_URL" =~ ^https?://[A-Za-z0-9._-]+(:[0-9]+)?$ ]]; then
  echo "!! --stt-url 形如 http://192.0.2.150:18090，当前: $STT_URL" >&2
  exit 1
fi
if [[ -n "$STT_TIMEOUT" && ! "$STT_TIMEOUT" =~ ^[0-9]+(ns|us|ms|s|m|h)$ ]]; then
  echo "!! --stt-timeout 为 Go duration（如 30s），当前: $STT_TIMEOUT" >&2
  exit 1
fi
if [[ -n "$STT_MAX_CHUNK_BYTES" && ! "$STT_MAX_CHUNK_BYTES" =~ ^[0-9]+$ ]]; then
  echo "!! --stt-max-chunk-bytes 必须是数字: $STT_MAX_CHUNK_BYTES" >&2
  exit 1
fi

# ---------- 二进制下载 ----------
# GitHub Release 产物命名: starburst-backend-{os}-{arch}
#   os ∈ linux|darwin，arch ∈ amd64|arm64|arm
# uname 输出与产物命名不同（x86_64→amd64、aarch64→arm64、armv7l→arm），需要显式映射
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  linux|darwin) ;;
  *) echo "!! 本脚本仅支持 Linux/Darwin，当前平台: $OS" >&2; exit 1 ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  armv7l|armv6l|arm) ARCH="arm" ;;
  *) echo "!! 不支持的架构: $ARCH" >&2; exit 1 ;;
esac

BIN_URL="${OCB_BIN_URL:-https://github.com/hiylo/starburst-backend/releases/latest/download/starburst-backend-${OS}-${ARCH}}"
if [[ "$SYSTEMD" -eq 1 ]]; then
  INSTALL_DIR="/usr/local/bin"
  CONFIG_DIR="/etc/starburst-backend"
  DATA_DIR="/var/lib/starburst-backend"
  SERVICE_FILE="/etc/systemd/system/starburst-backend.service"
else
  # 沙箱模式：全部落在 $PREFIX 下，便于非 root / CI 验证。
  INSTALL_DIR="$PREFIX/usr/local/bin"
  CONFIG_DIR="$PREFIX/etc/starburst-backend"
  DATA_DIR="$PREFIX/var/lib/starburst-backend"
  SERVICE_FILE="$PREFIX/etc/systemd/system/starburst-backend.service"
fi
BIN_PATH="$INSTALL_DIR/starburst-backend"

echo "==> 下载 $BIN_URL"
mkdir -p "$INSTALL_DIR"
if curl -fsSL -o "$BIN_PATH.tmp" "$BIN_URL"; then
  chmod +x "$BIN_PATH.tmp"
  mv "$BIN_PATH.tmp" "$BIN_PATH"
else
  echo "!! 下载失败。若在开发机本地运行，可先 go build -o $BIN_PATH ./cmd/starburst-backend 再重试" >&2
  exit 1
fi

echo "==> 写入配置 $CONFIG_DIR"
mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$(dirname "$SERVICE_FILE")"

# 生成启动参数。SQLite 数据放 /var/lib, Postgres 用连接串。
EXEC_ARGS=(--db "$DB" --workers "$WORKERS")
if [[ "$DB" == "sqlite" ]]; then
  EXEC_ARGS+=(--sqlite-path "$DATA_DIR/starburst-backend.db")
else
  EXEC_ARGS+=(--pg-dsn "$PG_DSN")
fi
[[ -n "$ADMIN_PASSWORD" ]] && EXEC_ARGS+=(--default-admin-password "$ADMIN_PASSWORD")
[[ -n "$DEFAULT_TOKEN" ]] && EXEC_ARGS+=(--default-token "$DEFAULT_TOKEN")
[[ -n "$TASK_RETENTION" ]] && EXEC_ARGS+=(--task-retention "$TASK_RETENTION")
[[ -n "$STT_URL" ]] && EXEC_ARGS+=(--stt-url "$STT_URL")
[[ -n "$STT_TIMEOUT" ]] && EXEC_ARGS+=(--stt-timeout "$STT_TIMEOUT")
[[ -n "$STT_MAX_CHUNK_BYTES" ]] && EXEC_ARGS+=(--stt-max-chunk-bytes "$STT_MAX_CHUNK_BYTES")

# systemd 按空白切分 ExecStart 的参数，含空格的值（DSN、密码）必须用双引号包裹；
# 值内部的双引号/反斜杠也要转义，否则会被 systemd 错误解析。
EXEC_QUOTED_ARGS=""
for arg in "${EXEC_ARGS[@]}"; do
  escaped="$(printf '%s' "$arg" | sed 's/[\\"]/\\&/g')"
  EXEC_QUOTED_ARGS="${EXEC_QUOTED_ARGS} \"${escaped}\""
done

# 动态值一律作为 printf 参数注入，不放进格式串，也不做 ${var//pat/rep} 替换：
# bash 的替换串里 & 会被还原成匹配到的占位符，DSN 里的 &（?sslmode=disable 等
# 查询参数）会被替换成 @EXEC_QUOTED_ARGS@ 从而写坏 ExecStart。
{
  printf '%s\n' \
    "[Unit]" \
    "Description=StarBurst Backend" \
    "After=network-online.target" \
    "Wants=network-online.target" \
    "" \
    "[Service]" \
    "Type=simple"
  printf 'ExecStart=%s --listen :%s %s\n' "$BIN_PATH" "$PORT" "$EXEC_QUOTED_ARGS"
  printf '%s\n' \
    "Restart=on-failure" \
    "RestartSec=5" \
    "User=root"
  printf 'Environment=OCB_OPENCODE_URL=%s\n' "${OCB_OPENCODE_URL:-http://127.0.0.1:4096}"
  printf 'WorkingDirectory=%s\n' "$DATA_DIR"
  printf '%s\n' "" "[Install]" "WantedBy=multi-user.target"
} > "$SERVICE_FILE"

if [[ "$SYSTEMD" -eq 1 ]]; then
  echo "==> 启动服务"
  systemctl daemon-reload
  systemctl enable starburst-backend
  systemctl restart starburst-backend
else
  echo "==> 沙箱模式：跳过 systemctl（unit 已写入 $SERVICE_FILE）"
fi

echo ""
echo "✔ 安装完成"
if [[ -n "$ADMIN_PASSWORD" ]]; then
  echo "  - 配置页: http://<本机IP>:$PORT/  (初始密码为你设置的 --admin-password)"
else
  echo "  - 配置页: http://<本机IP>:$PORT/  (默认密码 admin, 请首次登录后修改)"
fi
echo "  - 服务: starburst-backend (systemd, :$PORT)"
echo "  - 数据: $DATA_DIR"
echo "  - 日志: journalctl -u starburst-backend -f"
if [[ -n "$STT_URL" ]]; then
  echo "  - 语音识别: 已接入 $STT_URL"
else
  echo "  - 语音识别: 未配置（App 需要它在端侧模型不可用时兜底）"
  echo "    追加: systemctl edit starburst-backend 加 OCB_STT_URL，或重跑本脚本带 --stt-url"
fi
# App 侧默认按 opencode 同主机 :18880 推导 backend 地址；端口不一致时必须
# 在 App 的服务器配置里显式填 backendUrl，否则 App 找不到后端。
if [[ "$PORT" != "18880" ]]; then
  echo "  ! 端口是 :$PORT 而不是 18880：App 默认推导的是 :18880，请在 App"
  echo "    服务器配置里显式填写 backend 地址，或重装时加 --port 18880"
fi