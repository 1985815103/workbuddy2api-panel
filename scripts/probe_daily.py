#!/usr/bin/env python3
"""查询 4 个 workbuddy 账号的「每日/周期性」积分来源状态。

覆盖端点（全部只读 GET，不产生写操作）：
  - /activity/growth/streak              连登天数 + 档位奖励（7/14/28 天各档 credit/energy/卡/抽奖次数）
  - /activity/growth/lottery/summary     当前抽奖次数
  - /activity/growth/buddy/travel/status 猫猫旅行状态 + 到站可领 reward_credit
  - /portal/activity/school/tasks        开学季活动是否在期 + 任务状态

用法: python3 scripts/probe_daily.py
"""
from __future__ import annotations

import glob
import json
import os
import sys
import urllib.error
import urllib.request

AUTH_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "auths")
CHAT_CN = "https://copilot.tencent.com"
BILL_CN = "https://www.codebuddy.cn"
CHAT_GLOBAL = "https://www.workbuddy.ai"


def is_global(a: dict) -> bool:
    au = a.get("auth") or {}
    return (au.get("realm") or "").lower() == "global" or (au.get("domain") or "").endswith("workbuddy.ai")


def get(af: dict, base: str, path: str) -> tuple[int, dict | str]:
    au, acc = af.get("auth") or {}, af.get("account") or {}
    req = urllib.request.Request(base + path, method="GET")
    req.add_header("Authorization", "Bearer " + (au.get("accessToken") or ""))
    req.add_header("Accept", "application/json")
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", "WorkBuddy/5.5.4")
    req.add_header("X-CodeBuddy-Request", "1")
    req.add_header("Accept-Language", "en-US" if is_global(af) else "zh-CN")
    if acc.get("uid"):
        req.add_header("X-User-Id", acc["uid"])
    if acc.get("enterpriseId"):
        req.add_header("X-Enterprise-Id", acc["enterpriseId"])
        req.add_header("X-Tenant-Id", acc["enterpriseId"])
    if au.get("domain"):
        req.add_header("X-Domain", au["domain"])
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")[:200]
    except Exception as e:
        return 0, f"{type(e).__name__}: {e}"


def main() -> int:
    sys.stdout.reconfigure(encoding="utf-8")
    for path in sorted(glob.glob(os.path.join(AUTH_DIR, "workbuddy-*.json"))):
        af = json.load(open(path, encoding="utf-8"))
        uid = (af.get("account") or {}).get("uid", "?")
        nick = (af.get("account") or {}).get("nickname", "")
        chat = CHAT_GLOBAL if is_global(af) else CHAT_CN
        print("=" * 78)
        print(f"{uid[:8]}  {nick}")

        st, d = get(af, chat, "/activity/growth/streak")
        if isinstance(d, dict) and d.get("code") == 0:
            s = (d.get("data") or {}).get("streak") or {}
            mc = (d.get("data") or {}).get("makeup_cards") or {}
            rs = (d.get("data") or {}).get("redemption_status") or {}
            print(f"  连登: 已连续 {s.get('days')} 天  本月累计 {s.get('month_total_days')} 天  "
                  f"下档 {s.get('next_tier')}(还差 {s.get('next_tier_remaining')} 天)  补签卡 {mc.get('balance')}/{mc.get('max')}")
            print(f"  档位状态: 7d={rs.get('tier_7d_status')} 14d={rs.get('tier_14d_status')} 28d={rs.get('tier_28d_status')} 剩余 {rs.get('remaining_days')}")
            for t in rs.get("tiers") or []:
                print(f"    档位 {t.get('tier'):<4} 需 {t.get('days'):>2} 天 → "
                      f"{t.get('credit')} 积分 + {t.get('energy')} 能量 + {t.get('cards')} 补签卡 + {t.get('chances')} 抽奖")
        else:
            print(f"  连登: HTTP {st} {str(d)[:90]}")

        st, d = get(af, chat, "/activity/growth/lottery/summary")
        if isinstance(d, dict) and d.get("code") == 0:
            print(f"  抽奖次数: {json.dumps(d.get('data'), ensure_ascii=False)}")
        else:
            print(f"  抽奖: HTTP {st} {str(d)[:90]}")

        st, d = get(af, chat, "/activity/growth/buddy/travel/status")
        if isinstance(d, dict) and d.get("code") == 0:
            dd = d.get("data") or {}
            print(f"  旅行: state={dd.get('state')} 今日已派={dd.get('daily_limit_reached')} "
                  f"到站可领={dd.get('reward_credit')} 积分")
        else:
            print(f"  旅行: HTTP {st} {str(d)[:90]}")

        st, d = get(af, BILL_CN, "/portal/activity/school/tasks")
        if isinstance(d, dict):
            dd = d.get("data") or {}
            print(f"  开学季: in_period={dd.get('in_period')} code={d.get('code')} "
                  f"tasks={len(dd.get('tasks') or [])}")
        else:
            print(f"  开学季: HTTP {st} {str(d)[:90]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
