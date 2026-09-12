#!/usr/bin/env python3
"""本地流式语音识别服务（sherpa-onnx 流式 Zipformer 中英双语 int8）。

作为 opencode-backend 的识别引擎：客户端把 16kHz/16bit/单声道 PCM 按分片 POST，
服务端持续返回增量文本，松手后调用 finish 返回最终文本。全本地推理，无外部 API。

端点
  GET    /health                    健康检查、能力参数与当前会话数
  POST   /sessions                  创建识别会话，返回 session_id
  POST   /sessions/{id}/chunks      分片送入 PCM16LE 裸数据，返回增量文本
  POST   /sessions/{id}/finish      结束输入并返回最终文本（幂等，不删除会话）
  DELETE /sessions/{id}             丢弃会话

约束
  PCM16LE 单声道 16000Hz；单请求体不超过 --max-body-bytes（超限返回 413）；
  会话空闲超过 --idle-timeout 秒、或已 finish 的会话，15 秒后被回收；
  并发会话数上限 --max-sessions，达到上限时创建返回 503。

关键实现细节（都是踩过坑才有的）
  · sherpa-onnx 的 stateful encoder 单次 decode_stream 至少需要 39 帧特征（帧移 160 样本），
    即 6400 样本 / 400ms。更小的块会在 csrc/features.cc:GetFrames 直接 abort 整个进程，
    所以服务端先把任意大小分片攒在缓冲区里，再按 6400 样本为单位喂给识别器。
    客户端按 200ms 一片发送完全没问题，只是中间结果每两片才更新一次。
  · finish 时缓冲区余数补静音成整块，再额外喂 TAIL_CHUNKS 块静音，冲刷编码器回看窗口。
    不做这一步，最后停顿处的词（例如句尾"频繁的"）会被截掉。
  · 请求体超限时必须把剩余字节读完再回 413：HTTP/1.1 keep-alive 下如果留下未读完的
    请求体，代理方（Go 的连接池）复用这条连接时会读到乱码，表现为偶发 502 而不是 413。
  · 已 finish 的会话再送分片返回 409，而不是默默当成新内容继续解码。

锁约定
  Engine.lock 只保护 sessions 字典本身；Session.lock 保护单条流的解码。
  回收线程只标记并弹出会话，绝不在持有 Session.lock 之外去 decode_stream，
  这样不会出现两个线程同时解码同一条流。

部署（已在本项目 NAS 上验证：8 核 / 23G 内存，10 秒音频 RTF≈0.1，partial 延迟 20-45ms）
  # 1. 建虚拟环境（需 Python 3.9+，onnxruntime 有轮子）
  python3 -m venv /vol1/1000/stt-server/venv
  /vol1/1000/stt-server/venv/bin/pip install "sherpa-onnx>=1.13" numpy

  # 2. 下模型（约 197MB，国内用 hf-mirror，也可换回 huggingface.co）
  cd /vol1/1000/stt-server/model
  base=https://hf-mirror.com/csukuangfj/sherpa-onnx-streaming-zipformer-bilingual-zh-en-2023-02-20/resolve/main
  for f in encoder-epoch-99-avg-1.int8.onnx decoder-epoch-99-avg-1.int8.onnx \
           joiner-epoch-99-avg-1.int8.onnx tokens.txt; do
    curl -fL -o "$f" "$base/$f"
  done

  # 3. 常驻
  sudo cp stt-server.service /etc/systemd/system/
  sudo systemctl daemon-reload && sudo systemctl enable --now stt-server
  curl -s localhost:18090/health | python3 -m json.tool
  # {"status":"ok","model":...,"sample_rate":16000,"max_sessions":16,"model_load_seconds":7.86}

  # 4. 后端接入
  opencode-backend --stt-url http://192.0.2.150:18090

  # 5. 冒烟（把任意 16k/16bit/mono WAV 切片喂进去；不带 --token 直连引擎，带则经后端）
  python3 scripts/stt-server/smoke_test.py http://127.0.0.1:18090 sample.wav

  # 6. 自检（不需要模型，用假识别器验证 HTTP 层与分块逻辑）
  python3 scripts/stt-server/test_engine.py

常见坑
  · NAS 上若没有与 User 同名的组，systemd 报 status=216/GROUP，删掉 unit 里的 Group= 行即可。
  · 分片小于 400ms 时引擎不会立刻出字（内部攒够一个解码窗口才解），不是卡死。
  · 首次请求比后续慢（模型懒加载约 8s，已计入 model_load_seconds）。
  · 引擎与后端的最大分片必须一致：后端 --stt-max-chunk-bytes 不要大于引擎 --max-body-bytes，
    否则后端会把引擎的 413 变成 502。
"""

from __future__ import annotations

import argparse
import json
import os
import re
import signal
import struct
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import sherpa_onnx

SAMPLE_RATE = 16000
FEATURE_DIM = 80
# sherpa-onnx stateful encoder 单次 decode 的最小块长：39 帧特征 × 160 样本帧移。
DECODE_CHUNK = 6400
# 结束输入时额外补的静音块数，用于冲刷编码器回看窗口里的尾部词。
TAIL_CHUNKS = 2
# 回收线程的扫描间隔。
SWEEP_INTERVAL = 15
MODEL_LABEL = "sherpa-onnx-streaming-zipformer-bilingual-zh-en-int8"
ENCODER = "encoder-epoch-99-avg-1.int8.onnx"
DECODER = "decoder-epoch-99-avg-1.int8.onnx"
JOINER = "joiner-epoch-99-avg-1.int8.onnx"
TOKENS = "tokens.txt"
PATH_RE = re.compile(r"^/sessions/([^/]+)(/chunks|/finish)?$")


class SessionGone(Exception):
    """会话已被回收或删除。"""


class SessionClosed(Exception):
    """会话已 finish，不再接受分片。"""


class Session:
    """单个识别会话：一条在线流 + 互斥锁，防止分片交叉解码。"""

    def __init__(self, sid: str, recognizer, rate: int):
        self.sid = sid
        self.recognizer = recognizer
        self.rate = rate
        self.stream = recognizer.create_stream()
        self.lock = threading.Lock()
        self.last_used = time.time()
        self.finished = False
        self.reaped = False
        self.bytes_received = 0
        self.total_samples = 0
        self.text = ""
        self.pending: list[float] = []
        self.decodes = 0

    def accept(self, samples, nbytes: int) -> dict:
        """缓冲采样并按 DECODE_CHUNK 步进解码，返回增量结果。调用方不得持有 self.lock。"""
        with self.lock:
            if self.reaped:
                raise SessionGone(self.sid)
            if self.finished:
                raise SessionClosed(self.sid)
            self.last_used = time.time()
            self.bytes_received += nbytes
            self.total_samples += len(samples)
            self.pending.extend(samples)
            self._drain(len(self.pending))
            return {
                "session_id": self.sid,
                "text": self.text,
                "final": False,
                "bytes": self.bytes_received,
                "seconds": round(self.total_samples / self.rate, 3),
                "decode_chunk_ms": DECODE_CHUNK * 1000 // self.rate,
            }

    def finish(self) -> dict:
        """冲刷尾部采样与编码器回看窗口，返回最终结果。

        幂等：重复调用返回同一段文本。不删除会话，由客户端 DELETE 或回收线程清理。
        """
        with self.lock:
            if self.reaped:
                raise SessionGone(self.sid)
            if not self.finished:
                self.finished = True
                self.last_used = time.time()
                pad = (-len(self.pending)) % DECODE_CHUNK
                if pad:
                    self.pending.extend([0.0] * pad)
                    self._drain(len(self.pending))
                for _ in range(TAIL_CHUNKS):
                    self._decode([0.0] * DECODE_CHUNK)
                self.stream.input_finished()
                self.text = self._current_text()
        return {"session_id": self.sid, "text": self.text, "final": True}

    def _drain(self, n: int) -> None:
        """把缓冲区里满 DECODE_CHUNK 的部分依次送入识别器。"""
        for _ in range(n // DECODE_CHUNK):
            batch = self.pending[:DECODE_CHUNK]
            del self.pending[:DECODE_CHUNK]
            self._decode(batch)
        self.text = self._current_text()

    def _decode(self, block) -> None:
        """送入一个块并解码；返回的文本由 _current_text 统一取。"""
        self.stream.accept_waveform(self.rate, block)
        self.recognizer.decode_stream(self.stream)
        self.decodes += 1

    def _current_text(self) -> str:
        """返回当前累计文本（不含首尾空白）。

        get_result 在不同 sherpa-onnx 版本里既可能返回 str，也可能返回
        OnlineRecognitionResult，这里两种都兼容；解码动作由 decode_stream 触发。
        """
        result = self.recognizer.get_result(self.stream)
        if result is None:
            return ""
        text = result if isinstance(result, str) else getattr(result, "text", "")
        return str(text).strip()


class Engine:
    """识别引擎门面：管理会话生命周期与空闲回收。"""

    def __init__(self, model_dir: str, num_threads: int,
                 max_sessions: int, idle_timeout: float):
        model_dir = os.path.abspath(model_dir)
        self.model_dir = model_dir
        self.max_sessions = max_sessions
        self.idle_timeout = idle_timeout
        t0 = time.time()
        self.recognizer = sherpa_onnx.OnlineRecognizer.from_transducer(
            tokens=f"{model_dir}/{TOKENS}",
            encoder=f"{model_dir}/{ENCODER}",
            decoder=f"{model_dir}/{DECODER}",
            joiner=f"{model_dir}/{JOINER}",
            num_threads=num_threads,
            sample_rate=SAMPLE_RATE,
            feature_dim=FEATURE_DIM,
            provider="cpu",
            decoding_method="greedy_search",
            enable_endpoint_detection=False,
            debug=False,
        )
        self.sessions: dict[str, Session] = {}
        self.lock = threading.Lock()
        self.stopped = False
        self.model_load_seconds = round(time.time() - t0, 2)
        self.janitor = threading.Thread(target=self._sweep, daemon=True, name="stt-janitor")
        self.janitor.start()

    def create(self) -> str | None:
        """新建会话并返回 session_id；达到并发上限或 sid 碰撞时返回 None。"""
        for _ in range(8):
            sid = uuid.uuid4().hex[:16]
            with self.lock:
                if len(self.sessions) >= self.max_sessions:
                    return None
                if sid in self.sessions:
                    continue
                self.sessions[sid] = Session(sid, self.recognizer, SAMPLE_RATE)
                return sid
        return None

    def get(self, sid: str) -> Session | None:
        """取会话；不存在返回 None。"""
        with self.lock:
            return self.sessions.get(sid)

    def delete(self, sid: str) -> bool:
        """删除会话，返回是否命中。"""
        with self.lock:
            sess = self.sessions.pop(sid, None)
            if sess is None:
                return False
            sess.reaped = True
            return True

    def count(self) -> int:
        """当前会话数。"""
        with self.lock:
            return len(self.sessions)

    def _sweep(self) -> None:
        """每 15 秒回收已结束或空闲超时的会话，防止客户端中途退出后内存泄漏。"""
        while not self.stopped:
            time.sleep(SWEEP_INTERVAL)
            now = time.time()
            with self.lock:
                stale = [s for s in self.sessions.values()
                         if s.finished or now - s.last_used > self.idle_timeout]
                for sess in stale:
                    self.sessions.pop(sess.sid, None)
                    sess.reaped = True
            # 不在持有 Engine.lock 时做解码；这里只是摘掉字典引用。

    def stop(self) -> None:
        """停止回收线程。"""
        self.stopped = True


class Handler(BaseHTTPRequestHandler):
    """HTTP 处理器：把裸 PCM 分片送入引擎，返回 JSON 增量结果。"""

    engine: Engine
    max_body: int
    # 慢速客户端（声明了 Content-Length 却不发数据）不至于永久占住一个线程。
    timeout = 120

    def log_message(self, fmt, *args):
        pass  # 逐分片请求不打访问日志，避免淹没 journal

    def _json(self, code: int, payload: dict, close: bool = False) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        if close:
            # 必须显式宣告，代理方的连接池才会放弃复用这条连接。
            self.send_header("Connection", "close")
            self.close_connection = True
        self.end_headers()
        self.wfile.write(body)

    def _body(self, limit: int) -> tuple[bytes, int, bool]:
        """读请求体，返回 (body, 声明长度, 是否超限)。"""
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            length = 0
        if length <= 0:
            return b"", 0, False
        if length > limit:
            # 必须把请求体读干净再回 413，否则 keep-alive 连接会被残留字节污染。
            self.close_connection = True
            remaining = length
            while remaining > 0:
                block = self.rfile.read(min(65536, remaining))
                if not block:
                    break
                remaining -= len(block)
            return b"", length, True
        return self.rfile.read(length), length, False

    def do_GET(self):
        if self.path.split("?", 1)[0] != "/health":
            self._json(404, {"error": "not found"})
            return
        self._json(200, {
            "status": "ok",
            "model": MODEL_LABEL,
            "sample_rate": SAMPLE_RATE,
            "channels": 1,
            "sample_width": 2,
            "sessions": self.engine.count(),
            "max_sessions": self.engine.max_sessions,
            "max_body_bytes": self.max_body,
            "decode_chunk_ms": DECODE_CHUNK * 1000 // SAMPLE_RATE,
            "idle_timeout_seconds": self.engine.idle_timeout,
            "model_load_seconds": self.engine.model_load_seconds,
        })

    def do_DELETE(self):
        match = PATH_RE.match(self.path.split("?", 1)[0])
        if not match:
            self._json(404, {"error": "not found"})
            return
        sid = match.group(1)
        if self.engine.delete(sid):
            self._json(200, {"deleted": True, "session_id": sid})
        else:
            self._json(404, {"error": "session not found", "session_id": sid})

    def do_POST(self):
        raw = self.path.split("?", 1)[0]
        if raw == "/sessions":
            self._handle_create()
            return
        match = PATH_RE.match(raw)
        if not match or match.group(2) not in ("/chunks", "/finish"):
            self._json(404, {"error": "not found"})
            return
        sid, action = match.group(1), match.group(2)
        session = self.engine.get(sid)
        if session is None:
            self._json(404, {"error": "session not found", "session_id": sid})
            return

        if action == "/finish":
            try:
                self._json(200, session.finish())
            except SessionGone:
                self._json(404, {"error": "session not found", "session_id": sid})
            return

        body, declared, too_long = self._body(self.max_body)
        if too_long:
            self._json(413, {"error": "chunk too large", "max_bytes": self.max_body}, close=True)
            return
        if declared <= 0 or not body:
            self._json(400, {"error": "empty chunk", "session_id": sid})
            return
        samples = self._to_floats(body)
        if samples is None:
            self._json(400, {"error": "odd number of samples", "session_id": sid})
            return
        try:
            self._json(200, session.accept(samples, len(body)))
        except SessionGone:
            self._json(404, {"error": "session not found", "session_id": sid})
        except SessionClosed:
            self._json(409, {"error": "session already finished", "session_id": sid})

    @staticmethod
    def _to_floats(data: bytes):
        """PCM16LE 转归一化浮点；样本数为奇数时返回 None。"""
        if len(data) % 2 != 0:
            return None
        count = len(data) // 2
        if count == 0:
            return []
        ints = struct.unpack("<%dh" % count, data)
        return [v / 32768.0 for v in ints]

    def _handle_create(self):
        sid = self.engine.create()
        if sid is None:
            self._json(503, {"error": "too many sessions",
                             "max_sessions": self.engine.max_sessions})
            return
        self._json(201, {"session_id": sid, "sample_rate": SAMPLE_RATE,
                         "channels": 1, "sample_width": 2,
                         "decode_chunk_ms": DECODE_CHUNK * 1000 // SAMPLE_RATE})


def main() -> None:
    parser = argparse.ArgumentParser(description="本地流式语音识别服务")
    parser.add_argument("--host", default="0.0.0.0", help="监听地址")
    parser.add_argument("--port", type=int, default=18090, help="监听端口")
    parser.add_argument("--model-dir", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "model"),
                        help="模型目录（encoder/decoder/joiner/tokens）")
    parser.add_argument("--num-threads", type=int, default=4, help="ONNX 推理线程数")
    parser.add_argument("--max-sessions", type=int, default=16, help="并发会话上限")
    parser.add_argument("--idle-timeout", type=float, default=120.0, help="会话空闲回收秒数")
    parser.add_argument("--max-body-bytes", type=int, default=2 * 1024 * 1024, help="单分片最大字节")
    args = parser.parse_args()

    engine = Engine(args.model_dir, args.num_threads, args.max_sessions, args.idle_timeout)
    Handler.engine = engine
    Handler.max_body = args.max_body_bytes
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    server.daemon_threads = True
    server.allow_reuse_address = True

    def shutdown(signum, frame):
        engine.stop()
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    print("stt-server listening on %s:%d model=%s threads=%d load=%.2fs"
          % (args.host, args.port, args.model_dir, args.num_threads, engine.model_load_seconds), flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
