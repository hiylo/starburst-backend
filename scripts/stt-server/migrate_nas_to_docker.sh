#!/usr/bin/env bash
# 将 NAS 上 systemd 常驻的 stt-server（sherpa-onnx 流式识别引擎）
# 迁移为 docker 容器运行，并用私有 registry 镜像。
#
# 前置条件（在 NAS 上执行）：
#   1. NAS 已安装 docker；本机（或能访问 NAS 的机器）已构建镜像并 push 到：
#      192.0.2.150:5003/hiylo/stt-server:latest
#   2. 模型目录存在：/vol1/1000/stt-server/model
#      （含 encoder/decoder/joiner int8 onnx + tokens.txt，约 197MB）
#
# 回滚：~/stt-server.systemd.flag 记录了 systemd 是否原本启用，
#       失败时执行脚本末尾打印的回滚命令即可。

set -euo pipefail

REGISTRY="192.0.2.150:5003"
IMAGE="${REGISTRY}/hiylo/stt-server:latest"
CONTAINER="stt-server"
PORT="18090"
MODEL_DIR="/vol1/1000/stt-server/model"
SERVICE="stt-server.service"

echo "==> [1/5] 前置检查"
command -v docker >/dev/null || { echo "错误：NAS 未安装 docker"; exit 1; }
[ -d "$MODEL_DIR" ] || { echo "错误：模型目录不存在 $MODEL_DIR"; exit 1; }
echo "    模型目录 OK：$MODEL_DIR"
ls "$MODEL_DIR" | grep -qE "\.onnx$" || { echo "警告：$MODEL_DIR 下未发现 .onnx，确认模型完整"; }

echo "==> [2/5] 停用 systemd 服务（记录原状态便于回滚）"
if systemctl is-enabled --quiet "$SERVICE" 2>/dev/null; then
  echo "    记录：$SERVICE 原为 enabled"
  touch ~/stt-server.systemd.evict-flag
  systemctl disable --now "$SERVICE" || true
else
  echo "    记录：$SERVICE 原本未启用（可能是手动起的进程，需另行处理）"
  rm -f ~/stt-server.systemd.evict-flag
fi

echo "==> [3/5] 清理同名容器（若存在）"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

echo "==> [4/5] 拉取并启动容器"
docker pull "$IMAGE"
docker run -d --name "$CONTAINER" --restart unless-stopped \
  -p "${PORT}:${PORT}" \
  -v "${MODEL_DIR}:/opt/stt/model:ro" \
  "$IMAGE"

echo "==> [5/5] 健康检查"
for i in $(seq 1 15); do
  sleep 2
  if curl -sf "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    echo "    引擎就绪：" && curl -s "http://127.0.0.1:${PORT}/health"
    echo
    echo "完成。后端 --stt-url 无需改动（仍是 http://192.0.2.150:18090）。"
    exit 0
  fi
done
echo "错误：容器启动后 health 未通过，检查 docker logs $CONTAINER"
exit 1