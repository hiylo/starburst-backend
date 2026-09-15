#!/usr/bin/env python3
"""流式识别冒烟测试：把一段 16kHz/16bit/单声道 WAV 按 200ms 切片喂进去，
打印每个 partial 的 RTT、首个非空 partial 位置、最终文本与整体 RTF。

两种模式
  直连引擎（无 token）
    python3 smoke_test.py http://127.0.0.1:18090 sample.wav
  经 starburst-backend（带 token）
    python3 smoke_test.py http://192.0.2.150:18880 sample.wav --token ocb_xxx

只依赖标准库。RTF = 处理耗时 / 音频时长，越小越快（<1 表示快于实时）。
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.request
import wave

CHUNK_SAMPLES = 3200  # 200ms @16kHz，与手机端一致
SAMPLE_RATE = 16000


def call(base, path, token, method, data=None, timeout=60):
    """发一个请求，返回 (状态码, JSON 响应体)。非 2xx 不抛异常，交给调用方判断。"""
    headers = {"Content-Type": "application/octet-stream"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(base + path, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        try:
            return exc.code, json.loads(exc.read().decode("utf-8"))
        except ValueError:
            return exc.code, {"error": exc.read().decode("utf-8", "replace")}


def load_wav(path):
    """读 WAV 并校验格式，返回裸 PCM 字节。"""
    with wave.open(path) as wf:
        got = (wf.getframerate(), wf.getnchannels(), wf.getsampwidth())
        want = (SAMPLE_RATE, 1, 2)
        if got != want:
            sys.exit("WAV 格式不符: 实际 %s，需要 %s（16kHz 单声道 16bit）" % (got, want))
        return wf.readframes(wf.getnframes())


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("base", help="引擎或后端地址，如 http://127.0.0.1:18090")
    parser.add_argument("wav", help="待识别的 16k/16bit/mono WAV")
    parser.add_argument("--token", help="给了就走 starburst-backend 的 /api/stt 端点")
    args = parser.parse_args()

    base = args.base.rstrip("/")
    prefix = "/api/stt" if args.token else ""
    raw = load_wav(args.wav)

    code, body = call(base, prefix + "/sessions", args.token, "POST")
    if code < 200 or code >= 300:
        sys.exit("创建会话失败: %d %s" % (code, body))
    sid = body["session_id"]
    print("会话 %s" % sid)

    frames = len(raw) // (CHUNK_SAMPLES * 2)
    audio_seconds = len(raw) / 2 / SAMPLE_RATE
    print("音频 %.2fs，每 200ms 一片，共 %d 片" % (audio_seconds, frames))

    t0 = time.time()
    first_text_at = None
    for i in range(frames):
        chunk = raw[i * CHUNK_SAMPLES * 2:(i + 1) * CHUNK_SAMPLES * 2]
        t = time.time()
        code, body = call(base, prefix + "/sessions/%s/chunks" % sid, args.token, "POST", chunk)
        if code != 200:
            sys.exit("分片 %d 失败: %d %s" % (i, code, body))
        rtt = (time.time() - t) * 1000
        if first_text_at is None and body.get("text"):
            first_text_at = body.get("seconds")
            print("首个非空 partial 在 %.2fs 音频处（RTT %.1fms）" % (first_text_at, rtt))
        if i % 5 == 0 or i == frames - 1:
            print("  [%6.2fs] RTT%7.1fms  %s"
                  % (body.get("seconds", 0), rtt, body.get("text") or "(空)"))

    code, body = call(base, prefix + "/sessions/%s/finish" % sid, args.token, "POST")
    if code != 200:
        sys.exit("finish 失败: %d %s" % (code, body))
    total = time.time() - t0
    call(base, prefix + "/sessions/%s" % sid, args.token, "DELETE")

    print("\n最终文本: %s" % body.get("text", ""))
    print("总耗时 %.2fs，RTF=%.3f（<1 快于实时）" % (total, total / audio_seconds))
    if first_text_at is None:
        sys.exit("整段没有出过任何 partial，检查引擎与音频")


if __name__ == "__main__":
    main()
