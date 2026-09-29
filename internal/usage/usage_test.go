package usage

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
// 窗口口径：24h 窗口不含 100 天前的日桶；hours=0（全部历史）才含日点。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 1 || after.Totals.PromptTokens != 1 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/1（窗口外日桶不进聚合）", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 1 || after.Series[0].Scope != "hour" {
		t.Fatalf("series = %+v, want 仅当前小时 1 个点", after.Series)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 3 || all.Totals.PromptTokens != 15 {
		t.Fatalf("全部历史 totals = %d/%d, want 3/15", all.Totals.Requests, all.Totals.PromptTokens)
	}
	if len(all.Series) != 2 || all.Series[0].Scope != "day" || all.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", all.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(0, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// Snapshot 全口径窗口过滤：窗口外的数据不进**任何**聚合（卡片/表格/时序），
// 切窗口数字随之变化；hours=0 全部历史。Buckets 为窗口内命中的桶数。
func TestSnapshotWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.PromptTokens != 3 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/3（48h 前的数据应被过滤）", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 1 || s.Series[0].Scope != "hour" || s.Series[0].PromptTokens != 3 {
		t.Fatalf("series = %+v, want 仅窗口内 1 个小时点", s.Series)
	}
	if s.Buckets != 1 {
		t.Fatalf("buckets = %d, want 1（窗口内命中桶数）", s.Buckets)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 2 || all.Totals.PromptTokens != 8 {
		t.Fatalf("全部历史 totals = %d/%d, want 2/8", all.Totals.Requests, all.Totals.PromptTokens)
	}
	// since 是全库数据起点，不受窗口影响。
	if all.Since == "" || s.Since != all.Since {
		t.Fatalf("since 应为全库起点且不随窗口变化: all=%q windowed=%q", all.Since, s.Since)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// 积分消耗口径：只累计上报过 credit 的成功请求（CrN 样本数）；失败尝试与
// 未上报响应既不累计也不计样本；免费请求（credit=0）计样本不累计。
// 聚合沿既有链路透出：totals / by_model / by_account / 时序点。
func TestCreditAccumulateAndAggregate(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, Credit: 1.5, HasCredit: true}, true)
	r.Add(now, "cn", "u1", "glm-5.2", Delta{PromptTokens: 50, HasPromptTokens: true, Credit: 0.25, HasCredit: true}, true)
	// 免费请求：观测到 credit=0，样本数 +1、累计不变。
	r.Add(now, "cn", "u1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, Credit: 0, HasCredit: true}, true)
	// 未上报 credit：不计入累计也不计样本。
	r.Add(now, "cn", "u2", "glm-5.2", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	// 失败尝试：无 usage，天然无 credit。
	r.Add(now, "cn", "u2", "glm-5.2", Delta{}, false)

	s := r.Snapshot(24, nil)
	if s.Totals.Credits != 1.75 || s.Totals.CreditSamples != 3 {
		t.Fatalf("credits/samples = %v/%d, want 1.75/3", s.Totals.Credits, s.Totals.CreditSamples)
	}
	if len(s.ByModel) != 1 || s.ByModel[0].Credits != 1.75 || s.ByModel[0].CreditSamples != 3 {
		t.Fatalf("by_model credits = %+v", s.ByModel)
	}
	u1 := s.ByAccount[0]
	if u1.Key != "u1" || u1.Credits != 1.75 || u1.CreditSamples != 3 {
		t.Fatalf("by_account[0] = %+v, want u1 1.75/3", u1)
	}
	u2 := s.ByAccount[1]
	if u2.Key != "u2" || u2.Credits != 0 || u2.CreditSamples != 0 {
		t.Fatalf("by_account[1] = %+v, want u2 0/0（未上报不计样本）", u2)
	}
	if len(s.Series) != 1 || s.Series[0].Credits != 1.75 || s.Series[0].CreditSamples != 3 {
		t.Fatalf("series credits = %+v", s.Series)
	}
}

// Rollup 折叠保留积分累计与样本数（小时桶 → 日桶不丢口径）。
func TestRollupPreservesCredit(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true, Credit: 2.5, HasCredit: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true, Credit: 0, HasCredit: true}, true)

	r.Rollup(time.Now())
	all := r.Snapshot(0, nil)
	if all.Totals.Credits != 2.5 || all.Totals.CreditSamples != 2 {
		t.Fatalf("折叠后 credits/samples = %v/%d, want 2.5/2", all.Totals.Credits, all.Totals.CreditSamples)
	}
	if len(all.Series) != 1 || all.Series[0].Scope != "day" || all.Series[0].Credits != 2.5 {
		t.Fatalf("series = %+v, want 日点 credits=2.5", all.Series)
	}
}

// 旧版 usage.json（无 cr/crn/ch/cm/chn 字段）读入不报错：credit 与缓存维度都从 0
// 起步，前向兼容（老文件不该被新列读成"命中率 0%"）。
func TestLoadLegacyFileWithoutCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	scope := "h:" + time.Now().Format("2006-01-02T15")
	legacy := `{"version":1,"saved":"2026-01-01T00:00:00Z","buckets":[` +
		`{"s":"` + scope + `","r":"cn","u":"u1","m":"glm-5.2","q":2,"e":0,"p":100,"c":40,"t":140,"l":900,"ln":2,"v":12.5,"vn":2}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 2 || s.Totals.TotalTokens != 140 {
		t.Fatalf("旧文件 token 口径 = %d/%d, want 2/140", s.Totals.Requests, s.Totals.TotalTokens)
	}
	if s.Totals.Credits != 0 || s.Totals.CreditSamples != 0 {
		t.Fatalf("旧文件 credit 应从 0 起步: %v/%d", s.Totals.Credits, s.Totals.CreditSamples)
	}
	if s.Totals.CacheSamples != 0 || s.Totals.CacheHitRate != 0 {
		t.Fatalf("旧文件缓存维度应从 0 起步: %d/%v", s.Totals.CacheSamples, s.Totals.CacheHitRate)
	}
}

// 前缀缓存口径：命中/未命中累计 + 命中率，沿 credits 同一条链路透出到
// totals / by_model / by_account / 时序点。样本数区分「全未命中=0」与「未上报」。
func TestCacheAccumulateAndHitRate(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "deepseek-v4.1-flash", Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		CacheHitTokens: 900, CacheMissTokens: 100, HasCache: true,
		Credit: 0.9, HasCredit: true,
	}, true)
	r.Add(now, "cn", "u1", "deepseek-v4.1-flash", Delta{
		PromptTokens: 200, HasPromptTokens: true,
		CacheHitTokens: 100, CacheMissTokens: 100, HasCache: true,
	}, true)
	// 未上报缓存口径（老上游/失败尝试）：不计累计也不计样本。
	r.Add(now, "cn", "u1", "glm-5.3", Delta{PromptTokens: 50, HasPromptTokens: true}, true)
	// 全未命中：合法观测，计样本、命中数为 0。
	r.Add(now, "cn", "u2", "glm-5.3", Delta{
		PromptTokens: 60, HasPromptTokens: true,
		CacheHitTokens: 0, CacheMissTokens: 60, HasCache: true,
	}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.CacheHitTokens != 1000 || s.Totals.CacheMissTokens != 260 || s.Totals.CacheSamples != 3 {
		t.Fatalf("cache totals = %d/%d/%d, want 1000/260/3",
			s.Totals.CacheHitTokens, s.Totals.CacheMissTokens, s.Totals.CacheSamples)
	}
	if want := 1000.0 / 1260.0; math.Abs(s.Totals.CacheHitRate-want) > 1e-9 {
		t.Fatalf("hit rate = %v, want %v", s.Totals.CacheHitRate, want)
	}
	// 按模型：deepseek 行 1000/200，glm 行 0/60（glm 的未上报请求不进分母）。
	var ds, glm *KeyedAgg
	for i := range s.ByModel {
		switch s.ByModel[i].Key {
		case "deepseek-v4.1-flash":
			ds = &s.ByModel[i]
		case "glm-5.3":
			glm = &s.ByModel[i]
		}
	}
	if ds == nil || ds.CacheHitTokens != 1000 || ds.CacheMissTokens != 200 || ds.CacheSamples != 2 {
		t.Fatalf("by_model deepseek = %+v, want 1000/200/2", ds)
	}
	if glm == nil || glm.CacheHitTokens != 0 || glm.CacheMissTokens != 60 || glm.CacheSamples != 1 || glm.CacheHitRate != 0 {
		t.Fatalf("by_model glm = %+v, want 0/60/1 rate=0", glm)
	}
	// 按账号：u2 只有全未命中一条，命中率必须恰好为 0（不是"没数据"）。
	u2 := s.ByAccount[1]
	if u2.Key != "u2" || u2.CacheHitRate != 0 || u2.CacheSamples != 1 {
		t.Fatalf("by_account u2 = %+v, want rate=0 samples=1", u2)
	}
	if len(s.Series) != 1 || s.Series[0].CacheHitTokens != 1000 || s.Series[0].CacheMissTokens != 260 {
		t.Fatalf("series = %+v, want 1000/260", s.Series)
	}
}

// 无缓存样本时不产生命中率：只上报 credit 的请求不得被读成「命中率 0%」。
func TestCacheHitRateWithoutSamples(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true, Credit: 1, HasCredit: true}, true)
	s := r.Snapshot(24, nil)
	if s.Totals.CacheSamples != 0 || s.Totals.CacheHitRate != 0 {
		t.Fatalf("无样本时 samples/rate = %d/%v, want 0/0", s.Totals.CacheSamples, s.Totals.CacheHitRate)
	}
	if s.Totals.CacheHitTokens != 0 || s.Totals.CacheMissTokens != 0 {
		t.Fatalf("无样本时累计应保持 0: %d/%d", s.Totals.CacheHitTokens, s.Totals.CacheMissTokens)
	}
}

// Rollup 折叠保留缓存累计与样本数（小时桶 → 日桶不丢口径）。
func TestRollupPreservesCache(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true,
		CacheHitTokens: 7, CacheMissTokens: 0, HasCache: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true,
		CacheHitTokens: 0, CacheMissTokens: 3, HasCache: true}, true)

	r.Rollup(time.Now())
	all := r.Snapshot(0, nil)
	if all.Totals.CacheHitTokens != 7 || all.Totals.CacheMissTokens != 3 || all.Totals.CacheSamples != 2 {
		t.Fatalf("折叠后 cache = %d/%d/%d, want 7/3/2",
			all.Totals.CacheHitTokens, all.Totals.CacheMissTokens, all.Totals.CacheSamples)
	}
	if want := 0.7; math.Abs(all.Totals.CacheHitRate-want) > 1e-9 {
		t.Fatalf("折叠后 hit rate = %v, want %v", all.Totals.CacheHitRate, want)
	}
	if len(all.Series) != 1 || all.Series[0].Scope != "day" || all.Series[0].CacheHitTokens != 7 {
		t.Fatalf("series = %+v, want 日点 cache_hit=7", all.Series)
	}
}

// Daily 按自然日的全量聚合：同一天的多个小时桶折成一个日点（token/积分/请求数
// 求和），跨天分点且按日期升序。
func TestSnapshotDailyFoldsByDay(t *testing.T) {
	r := New("")
	now := time.Now()
	day := func(off, h int) time.Time {
		d := now.AddDate(0, 0, off)
		return time.Date(d.Year(), d.Month(), d.Day(), h, 0, 0, 0, time.Local)
	}
	// 昨天两个不同小时 + 今天一个：昨天应折成 1 个日点，不是 2 个。
	r.Add(day(-1, 0), "cn", "u", "m", Delta{PromptTokens: 10, HasPromptTokens: true, Credit: 1.5, HasCredit: true}, true)
	r.Add(day(-1, 3), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true, Credit: 0.5, HasCredit: true}, true)
	r.Add(day(0, 8), "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)

	s := r.Snapshot(0, nil)
	if len(s.Daily) != 2 {
		t.Fatalf("daily = %+v, want 2 个日点（昨天多个小时折成一天）", s.Daily)
	}
	if got := s.Daily[0]; got.Scope != "day" || got.T != day(-1, 0).Format(dayLayout) ||
		got.PromptTokens != 15 || got.Requests != 2 || got.CreditSamples != 2 {
		t.Fatalf("daily[0] = %+v, want 昨天 15pt/2req/2 样本", got)
	}
	if math.Abs(s.Daily[0].Credits-2) > 1e-9 {
		t.Fatalf("daily[0] credits = %v, want 2", s.Daily[0].Credits)
	}
	if got := s.Daily[1]; got.T != day(0, 0).Format(dayLayout) || got.PromptTokens != 7 {
		t.Fatalf("daily[1] = %+v, want 今天 7pt（升序）", got)
	}
}

// Daily 不随 hours 窗口变化：窗口外的数据不进卡片/表格，但仍出现在按天序列里
// （面板「按天」粒度自己按整日裁剪，后端不重复一套窗口口径）。
func TestSnapshotDailyIgnoresWindow(t *testing.T) {
	r := New("")
	now := time.Now()
	// 30 小时前：窗口起点 = 当前整点-23h > 现在-30h，恒在 24h 窗口之外（不依赖
	// 跑测试的钟点）。
	r.Add(now.Add(-30*time.Hour), "cn", "u", "m", Delta{PromptTokens: 100, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.PromptTokens != 5 {
		t.Fatalf("24h totals pt = %d, want 5（30 小时前的桶应在窗口外）", s.Totals.PromptTokens)
	}
	var sum int64
	for _, p := range s.Daily {
		sum += p.PromptTokens
	}
	if len(s.Daily) != 2 || sum != 105 {
		t.Fatalf("daily = %+v（合计 %d）, want 两天合计 105（不随窗口裁）", s.Daily, sum)
	}
	// hours=0 与窗口口径共享同一份 Daily。
	all := r.Snapshot(0, nil)
	if len(all.Daily) != len(s.Daily) || all.Daily[0].T != s.Daily[0].T {
		t.Fatalf("hours=0 daily = %+v, want 与 24h 同一份 %+v", all.Daily, s.Daily)
	}
}

// 维度口径（Filter）：只统计命中的桶；模型 ID 精确比对，不做前缀/模糊匹配。
// 同时验证下拉框选项列表**不受** filter 影响——否则选中一个模型后选项就只剩它。
func TestSnapshotFilterByModelAndUID(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "cn:glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u1", "cn:glm-5.2-thinking", Delta{PromptTokens: 100, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u2", "cn:glm-5.2", Delta{PromptTokens: 1000, HasPromptTokens: true}, true)

	s := r.SnapshotFiltered(24, map[string]string{"u1": "甲", "u2": "乙"}, Filter{Model: "cn:glm-5.2"})
	if s.Totals.PromptTokens != 1010 || s.Totals.Requests != 2 {
		t.Fatalf("按模型过滤 = %dpt/%dreq, want 1010/2（前缀相近的模型不得混入）",
			s.Totals.PromptTokens, s.Totals.Requests)
	}
	if len(s.ByModel) != 1 || s.ByModel[0].Key != "cn:glm-5.2" {
		t.Fatalf("by_model = %+v, want 只剩所选模型", s.ByModel)
	}
	if s.Buckets != 2 {
		t.Fatalf("buckets = %d, want 2（过滤后真正参与统计的桶数）", s.Buckets)
	}

	nicks := map[string]string{"u1": "甲", "u2": "乙"}
	both := r.SnapshotFiltered(24, nicks, Filter{Model: "cn:glm-5.2", UID: "u2"})
	if both.Totals.PromptTokens != 1000 || len(both.ByAccount) != 1 || both.ByAccount[0].Key != "u2" {
		t.Fatalf("模型+账号 AND 过滤 = %+v", both.Totals)
	}

	// 选项列表是全量：过滤后仍列出所有模型/账号，且昵称带上。
	if len(s.Models) != 2 || s.Models[0] != "cn:glm-5.2" || s.Models[1] != "cn:glm-5.2-thinking" {
		t.Fatalf("models 选项 = %v, want 全量且升序", s.Models)
	}
	if len(both.Models) != 2 {
		t.Fatalf("过滤不应裁剪模型选项: %v", both.Models)
	}
	if len(both.Accounts) != 2 || both.Accounts[0].UID != "u1" || both.Accounts[0].Nickname != "甲" {
		t.Fatalf("accounts 选项 = %+v, want u1(甲)/u2(乙)", both.Accounts)
	}
	// 空 Filter 与旧口径完全一致。
	plain := r.Snapshot(24, nil)
	if plain.Totals.PromptTokens != 1110 || len(plain.Models) != 2 || len(plain.Accounts) != 2 {
		t.Fatalf("空 Filter 口径 = %dpt/%dmodels/%daccounts, want 1110/2/2",
			plain.Totals.PromptTokens, len(plain.Models), len(plain.Accounts))
	}
}

// Rollup 的切点落在一个自然日中间时，该日会同时存在日桶（切点前的整点）与小时
// 桶（切点后的整点）——按天聚合必须把两者并成一天，而不是出两个同日期点。
// 这是生产上真实会出现的边界（hourlyKeep 精确到小时，切点几乎不会正好在 00:00）。
func TestSnapshotDailyMergesRolledAndLiveHours(t *testing.T) {
	r := New("")
	now := time.Now()
	// 把"现在"钉在当天 15:00，排除真实时钟让切点恰好落在整点/跨日的不确定性。
	mid := time.Date(now.Year(), now.Month(), now.Day(), 15, 0, 0, 0, time.Local)
	cutDay := mid.Add(-hourlyKeep)
	rolled := time.Date(cutDay.Year(), cutDay.Month(), cutDay.Day(), 3, 0, 0, 0, time.Local) // 切点前 → 日桶
	live := time.Date(cutDay.Year(), cutDay.Month(), cutDay.Day(), 20, 0, 0, 0, time.Local)  // 切点后 → 小时桶

	r.Add(rolled, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)
	r.Add(live, "cn", "u", "m", Delta{PromptTokens: 4, HasPromptTokens: true}, true)
	r.Rollup(mid)

	all := r.Snapshot(0, nil)
	// 先确认前提成立：确实折出了一个日桶、留着一个小时桶。
	if len(all.Series) != 2 || all.Series[0].Scope != "day" || all.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 1 日点(折叠) + 1 小时点(未折叠)", all.Series)
	}
	if len(all.Daily) != 1 || all.Daily[0].PromptTokens != 7 || all.Daily[0].Requests != 2 {
		t.Fatalf("daily = %+v, want 同日 1 点 7pt/2req", all.Daily)
	}
	if s := r.Snapshot(24, nil); len(s.Daily) != 1 || s.Daily[0].PromptTokens != 7 {
		t.Fatalf("24h daily = %+v, want 仍是完整 1 天 7pt", s.Daily)
	}
}
