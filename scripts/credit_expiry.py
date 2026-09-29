#!/usr/bin/env python3
"""列出 4 个 workbuddy 账号的积分包构成与真实到期时间（CycleEndTime）。

上游字段口径（与 internal/upstream/client.go 的 UserResourceDetailed 一致）：
  - 真到期字段是 CycleEndTime（"2006-01-02 15:04:05"，UTC+8），
    不是面板 /panel/api/packages 读的 ExpiredTime/PackageEndTime（实测恒空）。
  - remain/size 取 Cycle* 三字段（CycleCapacitySize>0 时），否则回退 Capacity*。

用法: python3 scripts/credit_expiry.py [--yes]
  默认 dry-run 只打印；--yes 无实际写操作（本脚本只读，参数仅为习惯一致）
"""
from __future__ import annotations

import glob
import json
import os
import sys
import urllib.request
from datetime import datetime, timedelta

AUTH_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "auths")
BILLING_CN = "https://www.codebuddy.cn"
BILLING_GLOBAL = "https://www.workbuddy.ai"
LAYOUT = "%Y-%m-%d %H:%M:%S"


def billing_base(af: dict) -> str:
    a = af.get("auth") or {}
    realm = (a.get("realm") or "").strip().lower()
    dom = (a.get("domain") or "").strip().lower()
    if realm == "global" or dom == "workbuddy.ai" or dom.endswith(".workbuddy.ai"):
        return BILLING_GLOBAL
    return BILLING_CN


def pack_remain_used(p: dict) -> tuple[int, int]:
    csize = p.get("CycleCapacitySize") or 0
    if csize > 0:
        remain = max(0, min(p.get("CycleCapacityRemain") or 0, csize))
        return remain, csize
    remain = p.get("CapacityRemain") or 0
    size = p.get("CapacitySize") or 0
    return remain, size


def fetch(af: dict) -> dict:
    now = datetime.now()
    body = json.dumps({
        "PageNumber": 1,
        "PageSize": 100,
        "ProductCode": "p_tcaca",
        "Status": [0, 3],
        "PackageEndTimeRangeBegin": now.strftime(LAYOUT),
        "PackageEndTimeRangeEnd": (now + timedelta(days=365 * 101)).strftime(LAYOUT),
    }).encode()
    base = billing_base(af)
    path = "/billing/meter/get-user-resource" if base == BILLING_GLOBAL else "/v2/billing/meter/get-user-resource"
    req = urllib.request.Request(base + path, data=body, method="POST")
    a, acc = af.get("auth") or {}, af.get("account") or {}
    req.add_header("Authorization", "Bearer " + (a.get("accessToken") or ""))
    req.add_header("Accept", "application/json")
    req.add_header("Content-Type", "application/json")
    # 上游 billing 域强校验的客户端标识头（与 internal/upstream/headers.go
    # 的 BillingHeaders 一致；缺 UA / X-CodeBuddy-Request 时直连返回 403）。
    req.add_header("User-Agent", "WorkBuddy/5.5.4")
    req.add_header("X-CodeBuddy-Request", "1")
    req.add_header("Accept-Language", "en-US" if "workbuddy.ai" in (a.get("domain") or "") else "zh-CN")
    if acc.get("uid"):
        req.add_header("X-User-Id", acc["uid"])
    if acc.get("enterpriseId"):
        req.add_header("X-Enterprise-Id", acc["enterpriseId"])
        req.add_header("X-Tenant-Id", acc["enterpriseId"])
    if a.get("domain"):
        req.add_header("X-Domain", a["domain"])
    with urllib.request.urlopen(req, timeout=30) as r:
        env = json.loads(r.read().decode("utf-8", "replace"))
    if env.get("code") != 0:
        raise SystemExit(f"code={env.get('code')} msg={env.get('msg')}")
    return env["data"]["Response"]["Data"]


def main() -> int:
    sys.stdout.reconfigure(encoding="utf-8")
    grand_remain = 0
    grand_size = 0
    rows_all: list[tuple] = []

    for path in sorted(glob.glob(os.path.join(AUTH_DIR, "workbuddy-*.json"))):
        af = json.load(open(path, encoding="utf-8"))
        uid = (af.get("account") or {}).get("uid", "?")
        nick = (af.get("account") or {}).get("nickname", "")
        data = fetch(af)
        packs = data.get("Accounts") or []
        remain = size = 0
        rows = []
        for p in packs:
            r, s = pack_remain_used(p)
            remain += r
            size += s
            end = (p.get("CycleEndTime") or "").strip()
            rows.append((end, r, s, (p.get("PackageName") or "")[:24],
                         (p.get("PackageCode") or "")[:22]))
        grand_remain += remain
        grand_size += size

        print("=" * 78)
        print(f"{uid[:8]}  {nick}   剩余 {remain} / 总额 {size}   包数 {len(packs)}")
        # 有到期时间的在前（按时间升序），无到期的在后
        rows.sort(key=lambda t: (t[0] == "", t[0]))
        for end, r, s, name, code in rows:
            tag = end if end else "(无到期时间)"
            print(f"   到期 {tag:<21} 剩 {r:>5}/{s:<5} {name:<26} {code}")
            rows_all.append((end, uid[:8], nick, r, s, name))

    print("=" * 78)
    print(f"四个号合计：剩余 {grand_remain} / 总额 {grand_size}")

    # 汇总：按到期时间聚合剩余积分
    print("\n---- 按到期时间聚合（仅列有到期的与无到期的剩余合计）----")
    buckets: dict[str, int] = {}
    for end, _uid8, _nick, r, _s, _name in rows_all:
        key = end if end else "(无到期时间)"
        buckets[key] = buckets.get(key, 0) + r
    for k in sorted(buckets, key=lambda x: (x == "(无到期时间)", x)):
        print(f"   {k:<21} 剩余合计 {buckets[k]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
