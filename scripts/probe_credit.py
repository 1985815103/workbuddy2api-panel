#!/usr/bin/env python3
"""探测单次请求的真实扣费（上游末帧 usage.credit）与 token 构成。

用于换算「N 积分能跑多少 token」：credit 与 prompt/completion 的对应关系由上游决定，
只有实测才能拿到权威单价（倍率牌价 ≠ 实际生效价，可能有限免/折扣）。

用法:
  python3 scripts/probe_credit.py --model cn:glm-5.3
  python3 scripts/probe_credit.py --model cn:glm-5.3 --prompt-repeat 60   # 放大 prompt 以分离输入/输出单价
"""
from __future__ import annotations

import argparse
import json
import sys
import urllib.request

BASE = "http://127.0.0.1:7863"
KEY = "test_key"


def probe(model: str, prompt: str, max_tokens: int, timeout: int = 180) -> dict:
    body = json.dumps({
        "model": model,
        "stream": True,
        "max_tokens": max_tokens,
        "stream_options": {"include_usage": True},
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request(
        BASE + "/v1/chat/completions", data=body, method="POST",
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json",
                 "Accept": "text/event-stream"})
    usage = None
    with urllib.request.urlopen(req, timeout=timeout) as r:
        for raw in r:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                break
            try:
                obj = json.loads(data)
            except Exception:
                continue
            if obj.get("usage"):
                usage = obj["usage"]
    return usage or {}


def main() -> int:
    sys.stdout.reconfigure(encoding="utf-8")
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True)
    ap.add_argument("--max-tokens", type=int, default=16)
    ap.add_argument("--prompt", default="说三个字")
    ap.add_argument("--prompt-repeat", type=int, default=0,
                    help="把一句填充语重复 N 次以放大 prompt")
    args = ap.parse_args()

    prompt = args.prompt
    if args.prompt_repeat > 0:
        filler = "这是一段用于放大输入长度的中性填充文本，不含任何指令。"
        prompt = (filler * args.prompt_repeat) + "\n忽略以上填充，只回三个字。"
    u = probe(args.model, prompt, args.max_tokens)
    if not u:
        print(f"{args.model}: 未取到 usage")
        return 1
    keep = {k: v for k, v in u.items() if not isinstance(v, dict)}
    print(f"model={args.model} max_tokens={args.max_tokens}")
    print(json.dumps(keep, ensure_ascii=False, indent=2))
    pt, ct, cr = u.get("prompt_tokens", 0), u.get("completion_tokens", 0), u.get("credit", 0)
    if pt or ct:
        print(f"--> credit={cr}  prompt={pt}  completion={ct}  total={u.get('total_tokens')}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
