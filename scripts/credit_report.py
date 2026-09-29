#!/usr/bin/env python3
"""积分分类报表：逐包拉取上游数据，按「来源类型」分类、按「过期时间」排序。

分类口径（按上游 PackageCode / SubProductCode，权威字段）：
  - 个人体验版   ：sp_tcaca_codebuddy_ide，周期包，CycleEndTime = 自然月末 23:59:59
  - 签到/任务奖励：TCACA_code_007_*（国内运营裂变包），有效期 = 到账 + 30 天
  - 拉新权益包   ：TCACA_code_030_*，邀请奖励（大额 30 天 / 邀请方小额 180 天）
  - 其他         ：其余包按上游原名展示

用法: python3 scripts/credit_report.py
"""
from __future__ import annotations

import glob
import json
import os
import sys
import urllib.request
from datetime import datetime, timedelta
from collections import defaultdict

AUTH_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "auths")
BILL_CN = "https://www.codebuddy.cn"
BILL_GLOBAL = "https://www.workbuddy.ai"
LAYOUT = "%Y-%m-%d %H:%M:%S"
NOW = datetime.now()


def is_global(af: dict) -> bool:
    a = af.get("auth") or {}
    return (a.get("realm") or "").lower() == "global" or (a.get("domain") or "").endswith("workbuddy.ai")


def classify(code: str, sub: str, size: int) -> str:
    c, s = (code or "").lower(), (sub or "").lower()
    if "tcaca_code_008" in c or "codebuddy_ide" == s.split("/")[-1] or "sp_tcaca_codebuddy_ide" == s:
        return "个人体验版(周期/月底清零)"
    if "tcaca_code_030" in c:
        return "拉新权益包(邀请奖励)"
    if "tcaca_code_007" in c:
        return "裂变包(签到/任务奖励)"
    return "其他"


def fetch(af: dict) -> list[dict]:
    now = datetime.now()
    body = json.dumps({
        "PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca", "Status": [0, 3],
        "PackageEndTimeRangeBegin": now.strftime(LAYOUT),
        "PackageEndTimeRangeEnd": (now + timedelta(days=365 * 101)).strftime(LAYOUT),
    }).encode()
    base = BILL_GLOBAL if is_global(af) else BILL_CN
    path = "/billing/meter/get-user-resource" if is_global(af) else "/v2/billing/meter/get-user-resource"
    req = urllib.request.Request(base + path, data=body, method="POST")
    a, acc = af.get("auth") or {}, af.get("account") or {}
    hdr = {
        "Authorization": "Bearer " + (a.get("accessToken") or ""), "Accept": "application/json",
        "Content-Type": "application/json", "User-Agent": "WorkBuddy/5.5.4",
        "X-CodeBuddy-Request": "1", "Accept-Language": "en-US" if is_global(af) else "zh-CN",
    }
    if acc.get("uid"):
        hdr["X-User-Id"] = acc["uid"]
    if acc.get("enterpriseId"):
        hdr["X-Enterprise-Id"] = acc["enterpriseId"]
        hdr["X-Tenant-Id"] = acc["enterpriseId"]
    if a.get("domain"):
        hdr["X-Domain"] = a["domain"]
    for k, v in hdr.items():
        req.add_header(k, v)
    d = json.loads(urllib.request.urlopen(req, timeout=30).read().decode())
    if d.get("code") != 0:
        raise SystemExit(f"code={d.get('code')} msg={d.get('msg')}")
    return d["data"]["Response"]["Data"]["Accounts"]


def pack_fields(p: dict) -> tuple[int, int]:
    csize = p.get("CycleCapacitySize") or 0
    if csize > 0:
        return max(0, min(p.get("CycleCapacityRemain") or 0, csize)), csize
    return p.get("CapacityRemain") or 0, p.get("CapacitySize") or 0


def main() -> int:
    sys.stdout.reconfigure(encoding="utf-8")
    rows = []
    for path in sorted(glob.glob(os.path.join(AUTH_DIR, "workbuddy-*.json"))):
        af = json.load(open(path, encoding="utf-8"))
        uid = (af.get("account") or {}).get("uid", "?")
        nick = (af.get("account") or {}).get("nickname", "")
        for p in fetch(af):
            remain, size = pack_fields(p)
            end = (p.get("CycleEndTime") or "").strip()
            rows.append({
                "uid": uid[:8], "nick": nick, "kind": classify(p.get("PackageCode"), p.get("SubProductCode"), size),
                "name": p.get("PackageName") or "", "code": (p.get("PackageCode") or "")[:18],
                "size": size, "remain": remain, "created": (p.get("CreatedAt") or "")[:19],
                "end": end or "(无到期)",
                "days": (datetime.strptime(end, LAYOUT) - NOW).total_seconds() / 86400 if end else None,
            })

    total = sum(r["remain"] for r in rows)
    print(f"生成时间 {NOW:%Y-%m-%d %H:%M:%S}   4 号剩余合计 {total} 分（共 {len(rows)} 个包）\n")

    # ── 视角一：按来源类型 ──
    print("=" * 96)
    print("【视角一】按来源类型分类")
    print("=" * 96)
    by_kind = defaultdict(lambda: [0, 0, 0])
    for r in rows:
        k = by_kind[r["kind"]]
        k[0] += r["remain"]
        k[1] += r["size"]
        k[2] += 1
    print(f"{'来源类型':<32}{'剩余':>8}{'总额':>8}{'包数':>6}{'占比':>8}")
    for kind, (rem, sz, n) in sorted(by_kind.items(), key=lambda x: -x[1][0]):
        print(f"{kind:<32}{rem:>8}{sz:>8}{n:>6}{rem/total*100:>7.1f}%")
    print(f"{'合计':<32}{total:>8}{sum(v[1] for v in by_kind.values()):>8}{len(rows):>6}")

    # ── 视角二：按过期日期（升序）──
    print()
    print("=" * 96)
    print("【视角二】按过期时间排序（越靠上越紧急）")
    print("=" * 96)
    print(f"{'过期时间':<21}{'剩余':>7}{'包数':>5}{'倒计时':>9}   来源（含账号分布）")
    by_date = defaultdict(lambda: [0, 0, defaultdict(int)])
    for r in rows:
        b = by_date[r["end"]]
        b[0] += r["remain"]
        b[1] += 1
        b[2][(r["kind"], r["uid"])] += r["remain"]
    cum = 0
    for end in sorted(by_date, key=lambda x: (x == "(无到期)", x)):
        rem, n, detail = by_date[end]
        cum += rem
        if end == "(无到期)":
            cd = "—"
        else:
            d = (datetime.strptime(end, LAYOUT) - NOW).total_seconds() / 86400
            cd = f"{d:.1f} 天"
        items = ", ".join(f"{k.split('(')[0]}@{u}×{v}" for (k, u), v in sorted(detail.items(), key=lambda x: -x[1]))
        print(f"{end:<21}{rem:>7}{n:>5}{cd:>9}   {items[:74]}")
    print(f"{'累计':<21}{cum:>7}")

    # ── 视角三：逐包明细（按过期时间排序）──
    print()
    print("=" * 96)
    print("【视角三】逐包明细（按过期时间升序）")
    print("=" * 96)
    print(f"{'过期时间':<21}{'账号':<10}{'来源类型':<26}{'面额':>6}{'剩余':>6}  {'到账时间':<20}")
    for r in sorted(rows, key=lambda x: (x["end"] == "(无到期)", x["end"], x["uid"])):
        print(f"{r['end']:<21}{r['uid']:<10}{r['kind']:<26}{r['size']:>6}{r['remain']:>6}  {r['created']:<20}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
