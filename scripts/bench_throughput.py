#!/usr/bin/env python3
"""对当前网关所有 /v1/models 模型做一次流式 token 生成速度基准.

测量指标（每模型一次调用）:
  - ttfb_ms   : 首个非空 SSE delta 到达耗时
  - total_ms  : 整个 SSE 流结束耗时
  - tokens    : 累计接收到的 delta 片段数（粗略近似 token 数）
  - chars     : 累计接收到的字符数（中文更直观）
  - chars_per_s / tokens_per_s : 以 total_ms - ttfb_ms 计算的生成速率
  - status / err : HTTP 状态 / 异常信息

用法
  python3 bench_throughput.py                          # 默认 http://127.0.0.1:7863, key=test_key
  python3 bench_throughput.py --base http://x:7863 --key sk-xxx
  python3 bench_throughput.py --models cn:kimi-k3-1,cn:glm-5.3
  python3 bench_throughput.py --max-tokens 200 --timeout 180 --save out.json
"""
from __future__ import annotations

import argparse
import json
import ssl
import sys
import time
import urllib.request
import urllib.error
from typing import Optional

DEFAULT_BASE = "http://127.0.0.1:7863"
DEFAULT_KEY = "test_key"
DEFAULT_PROMPT = "请用中文连续写一段不少于 120 字、不超过 200 字的科普短文，主题是「光的波粒二象性」。直接开始正文，不要解释、不要标题、不要列表。"


def log(msg: str) -> None:
    sys.stderr.write(msg + "\n")
    sys.stderr.flush()


def http_json(base: str, key: str, path: str, timeout: int = 15) -> dict:
    req = urllib.request.Request(
        base.rstrip("/") + path,
        headers={"Authorization": f"Bearer {key}", "Accept": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


def list_models(base: str, key: str) -> list[str]:
    d = http_json(base, key, "/v1/models")
    return [m["id"] for m in d.get("data", []) if m.get("id")]


def bench_one(
    base: str,
    key: str,
    model: str,
    prompt: str,
    max_tokens: int,
    timeout: int,
) -> dict:
    url = base.rstrip("/") + "/v1/chat/completions"
    payload = {
        "model": model,
        "stream": True,
        "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}],
    }
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=body,
        headers={
            "Authorization": f"Bearer {key}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        },
        method="POST",
    )

    result: dict = {
        "model": model,
        "http": None,
        "err": None,
        "ttfb_ms": None,
        "total_ms": None,
        "tokens": 0,           # content + reasoning_content 的 delta 片段总数
        "chars": 0,            # 正文 content 字符
        "rchars": 0,           # reasoning_content 字符
        "tokens_per_s": None,
        "chars_per_s": None,   # (chars + rchars) / 生成时长
    }

    t_start = time.monotonic()
    t_first: Optional[float] = None
    t_last: Optional[float] = None

    try:
        ctx = ssl.create_default_context()
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as resp:
            result["http"] = resp.status
            buf = b""
            while True:
                chunk = resp.read(4096)
                if not chunk:
                    break
                buf += chunk
                while b"\n" in buf:
                    line, buf = buf.split(b"\n", 1)
                    line = line.strip()
                    if not line or not line.startswith(b"data:"):
                        continue
                    data = line[5:].strip()
                    if data == b"[DONE]":
                        t_last = time.monotonic()
                        break
                    try:
                        obj = json.loads(data.decode("utf-8", "replace"))
                    except Exception:
                        continue
                    for ch in obj.get("choices", []) or []:
                        d = ch.get("delta") or {}
                        delta_c = d.get("content")
                        delta_r = d.get("reasoning_content")
                        if not delta_c and not delta_r:
                            continue
                        now = time.monotonic()
                        if t_first is None:
                            t_first = now
                        t_last = now
                        if delta_c:
                            result["tokens"] += 1
                            result["chars"] += len(delta_c)
                        if delta_r:
                            result["tokens"] += 1
                            result["rchars"] += len(delta_r)
                if t_last is not None and buf == b"":
                    # 给 [DONE] 之后留一点余地，让循环自然结束
                    pass
    except urllib.error.HTTPError as e:
        result["http"] = e.code
        try:
            result["err"] = e.read().decode("utf-8", "replace")[:300]
        except Exception:
            result["err"] = str(e)
    except Exception as e:  # noqa: BLE001
        result["err"] = f"{type(e).__name__}: {e}"

    t_end = time.monotonic()
    result["total_ms"] = round((t_end - t_start) * 1000, 1)
    if t_first is not None:
        result["ttfb_ms"] = round((t_first - t_start) * 1000, 1)
        gen_s = max(t_end - t_first, 1e-6)
        result["tokens_per_s"] = round(result["tokens"] / gen_s, 2)
        result["chars_per_s"] = round(
            (result["chars"] + result["rchars"]) / gen_s, 2
        )
    return result


def _f(v, width=7, prec=1):
    return f"{v:>{width}.{prec}f}" if isinstance(v, (int, float)) else f"{'-':>{width}}"


def fmt_row(r: dict) -> str:
    if r["err"]:
        return f"{r['model']:<28}  http={r['http']}  ERR: {r['err'][:80]}"
    return (
        f"{r['model']:<28}  "
        f"ttfb={_f(r['ttfb_ms'])} ms  "
        f"total={_f(r['total_ms'])} ms  "
        f"tok={r['tokens']:>4}  ch={r['chars']:>4}  rch={r.get('rchars', 0):>4}  "
        f"tok/s={_f(r['tokens_per_s'], 6, 2)}  ch/s={_f(r['chars_per_s'], 6, 2)}"
    )


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default=DEFAULT_BASE)
    ap.add_argument("--key", default=DEFAULT_KEY)
    ap.add_argument("--models", default="", help="逗号分隔；为空则拉 /v1/models 全部")
    ap.add_argument("--prompt", default=DEFAULT_PROMPT)
    ap.add_argument("--max-tokens", type=int, default=200)
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--rounds", type=int, default=1, help="每模型跑几轮，最终输出均值")
    ap.add_argument("--save", default="", help="把 JSON 结果写到文件")
    args = ap.parse_args()

    if args.models:
        models = [m.strip() for m in args.models.split(",") if m.strip()]
    else:
        log(f"[bench] GET {args.base}/v1/models ...")
        models = list_models(args.base, args.key)
    log(f"[bench] {len(models)} model(s) x {args.rounds} round(s); "
        f"max_tokens={args.max_tokens} timeout={args.timeout}s")

    # per-model 收集每轮结果
    rounds_results: dict[str, list[dict]] = {m: [] for m in models}
    for rnd in range(1, args.rounds + 1):
        log(f"[bench] ===== round {rnd}/{args.rounds} =====")
        for i, m in enumerate(models, 1):
            log(f"[bench] (r{rnd} {i}/{len(models)}) {m} ...")
            r = bench_one(args.base, args.key, m, args.prompt,
                          args.max_tokens, args.timeout)
            rounds_results[m].append(r)
            log("[bench]   " + fmt_row(r))

    # 聚合：每模型求均值（错误轮次跳过，但只要全错就保留 err）
    def avg(rs: list[dict], key: str):
        vals = [r[key] for r in rs if isinstance(r.get(key), (int, float))]
        return round(sum(vals) / len(vals), 2) if vals else None

    aggregated: list[dict] = []
    for m in models:
        rs = rounds_results[m]
        ok_rs = [r for r in rs if not r["err"]]
        if not ok_rs:
            aggregated.append({
                "model": m, "http": rs[-1]["http"],
                "err": "all rounds failed: " + (rs[-1]["err"] or "?"),
                "ttfb_ms": None, "total_ms": None,
                "tokens": 0, "chars": 0, "rchars": 0,
                "tokens_per_s": None, "chars_per_s": None,
                "rounds": rs,
            })
            continue
        aggregated.append({
            "model": m,
            "http": ok_rs[-1]["http"],
            "err": None,
            "ttfb_ms": avg(ok_rs, "ttfb_ms"),
            "total_ms": avg(ok_rs, "total_ms"),
            "tokens": round(avg(ok_rs, "tokens") or 0),
            "chars": round(avg(ok_rs, "chars") or 0),
            "rchars": round(avg(ok_rs, "rchars") or 0),
            "tokens_per_s": avg(ok_rs, "tokens_per_s"),
            "chars_per_s": avg(ok_rs, "chars_per_s"),
            "rounds": rs,
        })

    print(f"\n==== {args.rounds} 轮平均（按 tok/s 降序，错误排最后）====")
    ok = [r for r in aggregated if not r["err"]]
    bad = [r for r in aggregated if r["err"]]
    ok.sort(key=lambda r: (r["tokens_per_s"] or 0), reverse=True)
    for r in ok + bad:
        print(fmt_row(r))

    if args.save:
        with open(args.save, "w", encoding="utf-8") as f:
            json.dump({"base": args.base, "prompt": args.prompt,
                       "max_tokens": args.max_tokens,
                       "rounds": args.rounds,
                       "results": aggregated},
                      f, ensure_ascii=False, indent=2)
        print(f"\n[bench] saved -> {args.save}")

    return 0 if not bad else 1


if __name__ == "__main__":
    sys.exit(main())
