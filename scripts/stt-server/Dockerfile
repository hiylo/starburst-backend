# Local streaming speech recognition engine (sherpa-onnx zipformer zh-en int8).
# Containerises scripts/stt-server/stt_server.py so the NAS service no longer
# depends on a hand-managed venv + systemd unit.
#
# Build & push (on a machine with docker + NAS registry auth):
#   docker build -t 192.0.2.150:5003/hiylo/stt-server:latest scripts/stt-server
#   docker push   192.0.2.150:5003/hiylo/stt-server:latest
#
# Deploy (on the NAS, replaces systemd stt-server):
#   sudo systemctl disable --now stt-server
#   docker run -d --name stt-server --restart unless-stopped \
#     -p 18090:18090 \
#     -v /vol1/1000/stt-server/model:/opt/stt/model:ro \
#     192.0.2.150:5003/hiylo/stt-server:latest
#   sleep 5 && curl -s localhost:18090/health
#
# Compose alternative:
#   docker compose --profile stt up -d
#
# The model directory must contain encoder/decoder/joiner int8 onnx + tokens.txt.
# It is mounted read-only from the host so the ~197MB model is not baked into
# the image and can be re-downloaded without a rebuild.
# Base image comes from the NAS private registry (Docker Hub is unreachable in
# this sandbox); python 3.12 slim provides glibc which onnxruntime requires.
FROM 192.0.2.150:5003/library/python:3.12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# sherpa-onnx ships a prebuilt onnxruntime; wheels exist for 3.9-3.12.
# Use a domestic mirror so the layer builds on IPv4-only networks; the default
# pip index can stall when DNS hands out an IPv6 AAAA record first.
RUN pip install --no-cache-dir --upgrade pip \
 && pip install --no-cache-dir -i https://mirrors.aliyun.com/pypi/simple/ \
      "sherpa-onnx>=1.13" numpy

WORKDIR /opt/stt

COPY stt_server.py /opt/stt/stt_server.py

# Model files live on the host; only a directory placeholder is created here so
# the mount target exists even before the first volume attach.
RUN mkdir -p /opt/stt/model

# Non-root runtime, mirrors the old unit's User=hiylo intent.
RUN useradd --create-home --uid 1000 stt \
 && chown -R stt:stt /opt/stt
USER stt

EXPOSE 18090

ENTRYPOINT ["python", "/opt/stt/stt_server.py", "--host", "0.0.0.0", "--port", "18090", "--model-dir", "/opt/stt/model"]