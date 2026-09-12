#!/usr/bin/env python3
"""stt_server.py 的自检：用假识别器验证 HTTP 层与分块/flush 逻辑，不需要真实模型。

跑法
  python3 scripts/stt-server/test_engine.py

覆盖
  · 39 帧最短块：200ms 分片不触发解码、不崩，攒够 6400 样本才出字
  · finish 的静音补齐与尾部冲刷（decode 次数可断言）
  · finish 幂等、finish 后再送分片返回 409
  · 超大请求体返回干净的 413，且不污染 keep-alive 连接
  · 奇数字节 / 空分片 / 未知动作 / 重复 DELETE
  · 会话上限 503
"""

from __future__ import annotations

import http.client
import importlib.util
import json
import pathlib
import sys
import threading
import types
from http.server import ThreadingHTTPServer

HERE = pathlib.Path(__file__).resolve().parent

# ---- 假识别器：不依赖 sherpa_onnx，但保持相同的调用形态 ----

FAKE_CODE = """
class FakeStream:
    def __init__(self):
        self.samples = 0
        self.decodes = 0
        self.finished = False

    def accept_waveform(self, rate, samples):
        self.samples += len(samples)

    def input_finished(self):
        self.finished = True

class _Recognizer:
    def create_stream(self):
        return FakeStream()

    def decode_stream(self, stream, config=None):
        stream.decodes += 1

    def get_result(self, stream):
        return "词" * stream.decodes

class OnlineRecognizer:
    @staticmethod
    def from_transducer(**kwargs):
        return _Recognizer()
"""

fake = types.ModuleType("sherpa_onnx")
exec(FAKE_CODE, fake.__dict__)
sys.modules["sherpa_onnx"] = fake

spec = importlib.util.spec_from_file_location("stt_server", HERE / "stt_server.py")
stt_server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stt_server)

DECODE_CHUNK = stt_server.DECODE_CHUNK          # 6400 样本
WORD = "词"
_failures: list[str] = []
_checks = 0


def check(name: str, ok: bool, detail: str = "") -> None:
    global _checks
    _checks += 1
    if ok:
        print(f"  PASS  {name}")
    else:
        print(f"  FAIL  {name}  {detail}")
        _failures.append(name)


class Server:
    """一个跑在随机端口上的真实 HTTP 服务实例。"""

    def __init__(self, max_sessions: int = 4, max_body: int = 2 * 1024 * 1024):
        self.engine = stt_server.Engine("/tmp/opencode/stt/fake-model", 1,
                                        max_sessions, 120.0)
        stt_server.Handler.engine = self.engine
        stt_server.Handler.max_body = max_body
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), stt_server.Handler)
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.host, self.port = self.httpd.server_address

    def conn(self) -> http.client.HTTPConnection:
        return http.client.HTTPConnection(self.host, self.port, timeout=15)

    def call(self, conn, method, path, body=b"", headers=None):
        hdrs = dict(headers or {})
        hdrs.setdefault("Content-Length", str(len(body)))
        conn.request(method, path, body=body, headers=hdrs)
        resp = conn.getresponse()
        raw = resp.read()
        try:
            return resp.status, json.loads(raw.decode())
        except Exception:
            return resp.status, raw

    def close(self):
        self.engine.stop()
        self.httpd.shutdown()
        self.httpd.server_close()


def pcm(n_samples: int) -> bytes:
    """n_samples 个采样对应的裸字节（静音，内容不影响分块逻辑）。"""
    return b"\x01\x00" * n_samples


def test_main_path(srv: Server) -> None:
    print("\n[1] 分块缓冲与 39 帧最短块")
    c = srv.conn()
    st, health = srv.call(c, "GET", "/health")
    check("GET /health 200", st == 200, f"got {st}")
    for field in ("status", "model", "sample_rate", "max_sessions",
                  "max_body_bytes", "decode_chunk_ms", "model_load_seconds"):
        check(f"health 含 {field}", field in health, f"got {sorted(health)}")

    st, sess = srv.call(c, "POST", "/sessions")
    check("创建会话 201", st == 201, f"got {st} {sess}")
    sid = sess["session_id"]

    st, r = srv.call(c, "POST", f"/sessions/{sid}/chunks", pcm(3200))
    check("200ms 分片不触发解码（文本为空）",
          st == 200 and r["text"] == "" and r["bytes"] == 6400, f"got {st} {r}")

    st, r = srv.call(c, "POST", f"/sessions/{sid}/chunks", pcm(3200))
    check("攒够 6400 样本才出字", st == 200 and r["text"] == WORD, f"got {st} {r}")

    st, r = srv.call(c, "POST", f"/sessions/{sid}/chunks", pcm(3200))
    check("余下 3200 样本继续缓冲", st == 200 and r["text"] == WORD, f"got {st} {r}")

    st, r = srv.call(c, "POST", f"/sessions/{sid}/finish", pcm(0))
    check("finish 补静音 + 2 块尾部冲刷 = 4 次 decode",
          st == 200 and r["final"] is True and r["text"] == WORD * 4, f"got {st} {r}")

    st, r2 = srv.call(c, "POST", f"/sessions/{sid}/finish", pcm(0))
    check("finish 幂等", st == 200 and r2["text"] == r["text"], f"got {st} {r2}")

    st, r3 = srv.call(c, "POST", f"/sessions/{sid}/chunks", pcm(3200))
    check("finish 后再送分片返回 409", st == 409, f"got {st} {r3}")

    st, r4 = srv.call(c, "DELETE", f"/sessions/{sid}")
    check("DELETE 200", st == 200 and r4["deleted"] is True, f"got {st} {r4}")
    st, r5 = srv.call(c, "DELETE", f"/sessions/{sid}")
    check("重复 DELETE 404", st == 404, f"got {st} {r5}")
    c.close()


def test_bad_input(srv: Server) -> None:
    print("\n[2] 异常输入")
    c = srv.conn()
    st, sess = srv.call(c, "POST", "/sessions")
    sid = sess["session_id"]

    st, r = srv.call(c, "POST", f"/sessions/{sid}/chunks", b"\x01\x00\x01")
    check("奇数字节 400", st == 400 and "odd" in r["error"], f"got {st} {r}")

    st, r = srv.call(c, "POST", f"/sessions/{sid}/chunks", b"")
    check("空分片 400", st == 400 and "empty" in r["error"], f"got {st} {r}")

    st, r = srv.call(c, "POST", f"/sessions/{sid}/finish")
    check("空会话 finish 不崩", st == 200 and r["final"] is True, f"got {st} {r}")

    st, _ = srv.call(c, "POST", f"/sessions/{sid}/abort")
    check("未知动作 404", st == 404, f"got {st}")
    st, _ = srv.call(c, "POST", f"/sessions/{sid}/chunks/x")
    check("多一段路径 404", st == 404, f"got {st}")
    st, _ = srv.call(c, "GET", "/sessions")
    check("GET 非 health 404", st == 404, f"got {st}")
    st, _ = srv.call(c, "POST", "/sessions/0000000000000000/chunks", pcm(3200))
    check("未知会话 404", st == 404, f"got {st}")
    c.close()


def test_oversized_body(srv: Server) -> None:
    """回归用例：超大请求体必须回干净的 413，并宣告关闭连接，不能污染连接池。"""
    print("\n[3] 超大请求体与连接复用")
    c = srv.conn()
    st, sess = srv.call(c, "POST", "/sessions")
    sid = sess["session_id"]
    oversized = pcm(3000000)                     # 6MB，超过默认 2MiB 上限

    c.request("POST", f"/sessions/{sid}/chunks", body=oversized,
              headers={"Content-Type": "audio/pcm", "Content-Length": str(len(oversized))})
    try:
        resp = c.getresponse()
    except Exception as exc:
        check("6MB 分片拿到干净的 413", False, f"{type(exc).__name__}: {exc}")
        c.close()
        return

    payload = json.loads(resp.read().decode())
    check("6MB 分片返回 413（而不是连接中断）",
          resp.status == 413 and payload["max_bytes"] == 2 * 1024 * 1024,
          f"got {resp.status} {payload}")
    check("413 响应带 Connection: close",
          (resp.getheader("Connection") or "").lower() == "close",
          f"got {resp.getheader('Connection')!r}")
    c.close()

    st, r = srv.call(srv.conn(), "POST", f"/sessions/{sid}/chunks", pcm(3200))
    check("413 之后新连接可正常分片", st == 200 and r["session_id"] == sid, f"got {st} {r}")


def test_session_cap() -> None:
    print("\n[4] 会话上限")
    srv = Server(max_sessions=1)
    c = srv.conn()
    st, r1 = srv.call(c, "POST", "/sessions")
    check("第 1 个会话 201", st == 201, f"got {st} {r1}")
    st, r2 = srv.call(c, "POST", "/sessions")
    check("达到上限返回 503", st == 503 and "too many" in r2["error"], f"got {st} {r2}")
    c.close()
    srv.close()


def main() -> int:
    print(f"stt_server 自检：DECODE_CHUNK={DECODE_CHUNK} 样本"
          f"（{DECODE_CHUNK * 1000 // stt_server.SAMPLE_RATE}ms）")
    srv = Server()
    try:
        test_main_path(srv)
        test_bad_input(srv)
        test_oversized_body(srv)
    finally:
        srv.close()
    test_session_cap()

    print(f"\n{_checks} 项检查，{_checks - len(_failures)} 通过，{len(_failures)} 失败")
    if _failures:
        print("失败项: " + ", ".join(_failures))
        return 1
    print("全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
