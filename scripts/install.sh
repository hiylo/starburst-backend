#!/usr/bin/env bash
# opencode-backend 一键安装脚本
#
# 用法:
#   curl -fsSL https://<host>/install.sh | bash
# 或本地:
#   bash scripts/install.sh [--port 8080] [--db sqlite|postgres] [--pg-dsn "..."] [--admin-password "..."]
#
# 安装内容:
#   1. 下载 opencode-backend 单二进制到 /usr/local/bin
#   2. 写入 systemd 服务 (/etc/systemd/system/opencode-backend.service)
#   3. 生成默认配置 (/etc/opencode-backend/)
#   4. 启动并开机自启
#
#   通过环境变量/参数覆盖默认值:
#   OCB_PORT / --port
#   OCB_DB / --db (sqlite|postgres)
#   OCB_PG_DSN / --pg-dsn
#   OCB_ADMIN_PASSWORD / --admin-password
#   OCB_DEFAULT_TOKEN / --default-token (首次运行预置的 API token，客户端可免登录调用)
#   OCB_WORKERS / --workers (并发执行的任务数，默认 4)
#   OCB_PREFIX / --prefix
#     安装根目录，默认 /（即 /usr/local/bin、/etc/...、/var/lib/...）。
#     设成非根目录即"沙箱模式"：所有文件写到该目录下、跳过 systemctl，
#     用于非 root 或 CI 里验证脚本，不动真实系统。例:
#       OCB_BIN_URL=file:///tmp/ocb bash scripts/install.sh --prefix /tmp/sandbox
set -euo pipefail

# ---------- 参数解析 ----------
PORT="${OCB_PORT:-8080}"
DB="${OCB_DB:-sqlite}"
PG_DSN="${OCB_PG_DSN:-}"
ADMIN_PASSWORD="${OCB_ADMIN_PASSWORD:-}"
DEFAULT_TOKEN="${OCB_DEFAULT_TOKEN:-}"
WORKERS="${OCB_WORKERS:-4}"
PREFIX="${OCB_PREFIX:-/}"

while [[ $# -gt 0 ]]; do
  opt="$1"
  case "$opt" in
    -h|--help)
      echo "用法: $0 [--port 8080] [--db sqlite|postgres] [--pg-dsn dsn] [--admin-password pw] [--default-token tok] \\"
      echo "       [--workers 4] [--prefix /]"
      exit 0 ;;
    --port|--db|--pg-dsn|--admin-password|--default-token|--workers|--prefix)
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
        --prefix) PREFIX="$2" ;;
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

# ---------- 二进制下载 ----------
# GitHub Release 产物命名: opencode-backend-{os}-{arch}
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

BIN_URL="${OCB_BIN_URL:-https://github.com/hiylo/opencode-backend/releases/latest/download/opencode-backend-${OS}-${ARCH}}"
if [[ "$SYSTEMD" -eq 1 ]]; then
  INSTALL_DIR="/usr/local/bin"
  CONFIG_DIR="/etc/opencode-backend"
  DATA_DIR="/var/lib/opencode-backend"
  SERVICE_FILE="/etc/systemd/system/opencode-backend.service"
else
  # 沙箱模式：全部落在 $PREFIX 下，便于非 root / CI 验证。
  INSTALL_DIR="$PREFIX/usr/local/bin"
  CONFIG_DIR="$PREFIX/etc/opencode-backend"
  DATA_DIR="$PREFIX/var/lib/opencode-backend"
  SERVICE_FILE="$PREFIX/etc/systemd/system/opencode-backend.service"
fi
BIN_PATH="$INSTALL_DIR/opencode-backend"

echo "==> 下载 $BIN_URL"
mkdir -p "$INSTALL_DIR"
if curl -fsSL -o "$BIN_PATH.tmp" "$BIN_URL"; then
  chmod +x "$BIN_PATH.tmp"
  mv "$BIN_PATH.tmp" "$BIN_PATH"
else
  echo "!! 下载失败。若在开发机本地运行，可先 go build -o $BIN_PATH ./cmd/opencode-backend 再重试" >&2
  exit 1
fi

echo "==> 写入配置 $CONFIG_DIR"
mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$(dirname "$SERVICE_FILE")"

# 生成启动参数。SQLite 数据放 /var/lib, Postgres 用连接串。
EXEC_ARGS=(--db "$DB" --workers "$WORKERS")
if [[ "$DB" == "sqlite" ]]; then
  EXEC_ARGS+=(--sqlite-path "$DATA_DIR/opencode-backend.db")
else
  EXEC_ARGS+=(--pg-dsn "$PG_DSN")
fi
[[ -n "$ADMIN_PASSWORD" ]] && EXEC_ARGS+=(--default-admin-password "$ADMIN_PASSWORD")
[[ -n "$DEFAULT_TOKEN" ]] && EXEC_ARGS+=(--default-token "$DEFAULT_TOKEN")

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
    "Description=OpenCode Backend" \
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
  systemctl enable opencode-backend
  systemctl restart opencode-backend
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
echo "  - 服务: opencode-backend (systemd, :$PORT)"
echo "  - 数据: $DATA_DIR"
echo "  - 日志: journalctl -u opencode-backend -f"