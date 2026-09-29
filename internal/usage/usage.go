// Package usage 记录并聚合逐请求 token 用量与积分消耗，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model, rate) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 积分消耗口径：逐请求**实测扣费**（上游成功响应末帧/聚合体的 usage.credit），
// 失败尝试与未上报 credit 的响应不计入；CrN（样本数）区分「全免费=0」与「未上报」。
// 与「积分构成」页的余额快照（含签到赠送/过期作废/对账延迟）是两套口径，不强行对平。
//
// 前缀缓存口径（Ch/Cm/ChN）：上游按请求下发 prompt_cache_hit_tokens /
// prompt_cache_miss_tokens。缓存命中的 prompt token 计价远低于全价——实测
// deepseek-v4.1-flash 为**完全免费**、glm-5.3 约为全价的 25%——因此「积分 ÷ 合计
// token」在模型之间不可比（分母把命中与未命中按同权重相加，分子却不含命中那部分
// 费用）。曾有一次误读：GLM-5.3 与 DeepSeek-V4.1-Flash 盘面倍率差 7.2 倍，积分却
// 差 72 倍（11.1 倍真实牌价 × 6.5 倍缓存经济性）。这一维度就是用来拆开这个差值的，
// 面板据此显示命中率与实付单价。ChN 同 CrN 口径，区分「全未命中=0」与「未上报」。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// fileVersion 是 usage.json 的当前格式版本。版本 2 增加积分观测字段，版本 3
// 增加模型生效倍率分区；旧版本缺失字段按零值加载，旧数据不会丢弃。
const fileVersion = 3

// bucket 一个 (时间片, realm, uid, model, rate) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string  `json:"s"`            // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string  `json:"r"`            // cn / global
	UID   string  `json:"u"`            // 账号 uid
	Model string  `json:"m"`            // 上游裸模型名
	Rate  string  `json:"x,omitempty"`  // 请求时生效积分倍率（规范化数值；旧桶为空）
	Req   int64   `json:"q"`            // 请求数（含失败）
	Err   int64   `json:"e"`            // 失败数
	PT    int64   `json:"p"`            // prompt tokens
	CT    int64   `json:"c"`            // completion tokens
	TT    int64   `json:"t"`            // total tokens（上游给什么用什么的合计）
	LatMs int64   `json:"l"`            // 延迟累计（ms）
	LatN  int64   `json:"ln"`           // 延迟样本数
	TPS   float64 `json:"v"`            // 吐字速率累计
	TPSN  int64   `json:"vn"`           // 速率样本数
	CR    float64 `json:"cr,omitempty"` // usage.credit 累计（仅明确存在的观测）
	CRN   int64   `json:"cn,omitempty"` // usage.credit 样本数（区分缺字段与真实 0）
	CRT   int64   `json:"ct,omitempty"` // 同时具备 credit 与 token 的 Token 合计
	Ch    int64   `json:"ch"`           // 前缀缓存命中的 prompt tokens
	Cm    int64   `json:"cm"`           // 前缀缓存未命中的 prompt tokens（全价）
	ChN   int64   `json:"chn"`          // 缓存样本数（区分「全未命中=0」与「未上报」）
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model|rate
	dirty   bool
	started time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	// Credit 本次尝试的实测扣费积分（上游 usage.credit）。HasCredit=false 表示
	// 上游未上报（失败尝试或个别模型），不计入累计也不计入样本数。
	Credit          float64
	HasCredit       bool
	ModelRate       string
	LatencyMs       int64
	HasLatency      bool
	TokensPerSecond float64
	HasTPS          bool
	// CacheHitTokens/CacheMissTokens 上游前缀缓存口径。HasCache=false 表示本次
	// 尝试未上报（失败尝试、老版上游），不计入累计也不计入样本数。
	CacheHitTokens  int64
	CacheMissTokens int64
	HasCache        bool
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	scope := "h:" + now.Format(hourLayout)
	key := scope + "|" + realm + "|" + uid + "|" + model + "|" + d.ModelRate

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model, Rate: d.ModelRate}
		r.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasCredit {
		b.CR += d.Credit
		b.CRN++
		// 比例只使用同一次请求同时具备 credit 与 token 的样本，避免把
		// 仅 token 的旧记录或仅 credit 的观测混进分母。
		if d.HasTotal {
			b.CRT += d.TotalTokens
		} else if d.HasPromptTokens || d.HasCompletion {
			b.CRT += d.PromptTokens + d.CompletionTokens
		}
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	if d.HasCache {
		b.Ch += d.CacheHitTokens
		b.Cm += d.CacheMissTokens
		b.ChN++
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model + "|" + b.Rate})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
			dst.CR += src.CR
			dst.CRN += src.CRN
			dst.CRT += src.CRT
			dst.Ch += src.Ch
			dst.Cm += src.Cm
			dst.ChN += src.ChN
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model+"|"+b.Rate] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: fileVersion, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests           int64   `json:"requests"`
	Errors             int64   `json:"errors"`
	PromptTokens       int64   `json:"prompt_tokens"`
	CompletionTok      int64   `json:"completion_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	Credits            float64 `json:"credits"`
	CreditSamples      int64   `json:"credit_samples"`
	CreditTokens       int64   `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
	AvgLatencyMs       float64 `json:"avg_latency_ms"`
	AvgTPS             float64 `json:"avg_tokens_per_second"`
	// CacheHitTokens/CacheMissTokens 窗口内上游前缀缓存口径的 prompt token 累计；
	// CacheHitRate 是命中占两者之和的比例（无样本时为 0，由 CacheSamples 判空）。
	// CacheSamples=上报过缓存口径的请求数。
	CacheHitTokens  int64   `json:"cache_hit_tokens"`
	CacheMissTokens int64   `json:"cache_miss_tokens"`
	CacheSamples    int64   `json:"cache_samples"`
	CacheHitRate    float64 `json:"cache_hit_rate"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.Credits += b.CR
	g.CreditSamples += b.CRN
	g.CreditTokens += b.CRT
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
	g.CacheHitTokens += b.Ch
	g.CacheMissTokens += b.Cm
	g.CacheSamples += b.ChN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	if g.CreditTokens > 0 {
		a.CreditsPer1MTokens = g.Credits / float64(g.CreditTokens) * 1_000_000
	}
	// 命中率分母只用命中+未命中：两者成对上报，0/0 的退化观测不产生命中率。
	if den := a.CacheHitTokens + a.CacheMissTokens; den > 0 {
		a.CacheHitRate = float64(a.CacheHitTokens) / float64(den)
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // 账号行放昵称
	Agg
}

// Point 时序上的一个点。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// CreditAgg 积分扣除统计的一行。Key 在账号维度是 UID，在模型维度是裸模型名；
// Rate 仅模型维度使用；比例分母只统计与 credit 同时存在的 Token 样本。
type CreditAgg struct {
	Key                string  `json:"key"`
	Realm              string  `json:"realm,omitempty"`
	Nickname           string  `json:"nickname,omitempty"`
	Rate               string  `json:"rate,omitempty"`
	Requests           int64   `json:"requests"`
	Credits            float64 `json:"credits"`
	CreditSamples      int64   `json:"credit_samples"`
	CreditTokens       int64   `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
}

type creditAcc struct {
	CreditAgg
}

func (a *creditAcc) add(b *bucket) {
	a.Requests += b.Req
	a.Credits += b.CR
	a.CreditSamples += b.CRN
	a.CreditTokens += b.CRT
}

func (a *creditAcc) finish() CreditAgg {
	out := a.CreditAgg
	if a.CreditTokens > 0 {
		out.CreditsPer1MTokens = a.Credits / float64(a.CreditTokens) * 1_000_000
	}
	return out
}

// Filter 用量视图的维度口径：空串 = 不限；模型与账号同时给出时按 AND 生效。
// 取值与桶内字段**精确**比对（模型是上游 id 原文，如 "cn:glm-5.2"）——面板下拉框
// 给的就是精确 id，模糊匹配会让"选了 glm-5.2 却把 glm-5.2-thinking 也算进来"这类
// 偏口径蒙混过关。
type Filter struct {
	Model string
	UID   string
}

// AccountOption 账号下拉选项（uid + 昵称）；昵称仅用于展示。
type AccountOption struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals    Agg        `json:"totals"`
	ByRealm   []KeyedAgg `json:"by_realm"`
	ByAccount []KeyedAgg `json:"by_account"`
	ByModel   []KeyedAgg `json:"by_model"`
	Series    []Point    `json:"series"`
	// Daily 按自然日聚合的**全量**时序（小时桶折算到所属日期），供面板「按天」
	// 粒度看逐日用量。与 Series 的关键差别：它不随 hours 窗口变化——前端把窗口
	// 按**整日**口径裁剪（近 N 小时 → 含今天在内的 ceil(N/24) 个自然日），所以
	// 无论窗口选多细，「按天」视图都是一条完整的日序列，也把 90 天前折叠出的
	// 日桶一并纳入（日桶只在那里存在）。维度口径与其它口径一致（受 Filter 影响）。
	Daily []Point `json:"daily"`
	// Models/Accounts 是下拉框的维度选项：库里有数据就列出，**不受 Filter 与
	// 窗口影响**。理由：选项若跟着窗口缩水，选中的维度会在切窗口时无声消失，
	// 用户随即把聚合数字误读成该维度的数字——宁可让选中项在空窗口下显示 0。
	Models   []string        `json:"models"`
	Accounts []AccountOption `json:"accounts"`
	// CreditByAccount/CreditByModel 积分扣除维度统计（倍率分组口径见 CreditAgg）。
	CreditByAccount []CreditAgg `json:"credit_by_account"`
	CreditByModel   []CreditAgg `json:"credit_by_model"`
	Buckets         int         `json:"buckets"`
	FileBytes       int64       `json:"file_bytes"`
	Since           string      `json:"since,omitempty"`
	Generated       string      `json:"generated"`
}

// dayOf 取一个分片所属的自然日（本地时区，与分片键同口径）：小时桶折算到
// 所属日期，日桶本就是一天。解析失败的脏桶返回空串，不参与按天聚合。
func dayOf(scope string) string {
	if strings.HasPrefix(scope, "d:") {
		d := strings.TrimPrefix(scope, "d:")
		if _, err := time.ParseInLocation(dayLayout, d, time.Local); err != nil {
			return ""
		}
		return d
	}
	if !strings.HasPrefix(scope, "h:") {
		return ""
	}
	ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(scope, "h:"), time.Local)
	if err != nil {
		return ""
	}
	return ts.Format(dayLayout)
}

// points 把 (分片键 → 累计器) 转成按 key 升序的时序点。key 的字典序即时间序
// （同格式同前缀内成立），因此不需要额外解析时间。
func points(m map[string]*aggAcc, scope string) []Point {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		out = append(out, Point{T: k, Scope: scope, Agg: m[k].finish()})
	}
	return out
}

// sortedKeys 把集合转成升序切片（下拉框选项要稳定顺序，map 遍历顺序会抖）。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Snapshot 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// hours>0：窗口 = [当前整点-(hours-1)小时, now]，卡片汇总/按域/按账号/按模型/
// 时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化（曾长期是"卡片为
// 全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没生效"，已废弃）。
// 小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，故小时窗口
// 天然不含更早的日桶。
// hours<=0：全部历史（含已折叠日桶），供「全部历史」选项看长期趋势。
//
// 唯一的例外是 Daily：它恒为全量按天序列（见字段注释），窗口由前端按整日裁剪。
//
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	return r.snapshot(hours, nicks, Filter{}, nil)
}

// SnapshotFiltered 同 Snapshot，但只统计符合 filter 的桶（空 Filter = 不限）。
// 口径作用于卡片/按域/按账号/按模型/时序/按天**全部**聚合，唯一不受它影响的是
// 下拉框选项列表（见 Snapshot 字段注释）。Buckets 计数也随之变成"过滤后命中的
// 桶数"——它描述的是这次统计真正用了多少桶，不是窗口里有多少桶。
func (r *Recorder) SnapshotFiltered(hours int, nicks map[string]string, f Filter) Snapshot {
	return r.snapshot(hours, nicks, f, nil)
}

// SnapshotWithRates 与 Snapshot 相同，但允许为缺少历史倍率的旧桶提供当前
// 模型倍率回填。currentRate 返回空串时该行按“未知倍率”聚合，不伪造价格。
func (r *Recorder) SnapshotWithRates(hours int, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	return r.snapshot(hours, nicks, Filter{}, currentRate)
}

// SnapshotFilteredWithRates 维度过滤与倍率回填同时生效（面板 usage 接口：
// 既要 model/uid 下拉过滤，也要旧桶的当前倍率回填）。
func (r *Recorder) SnapshotFilteredWithRates(hours int, nicks map[string]string, f Filter, currentRate func(realm, model string) string) Snapshot {
	return r.snapshot(hours, nicks, f, currentRate)
}

// snapshot 全参数内核：维度过滤（f）+ 倍率回填（currentRate）同时生效。
func (r *Recorder) snapshot(hours int, nicks map[string]string, f Filter, currentRate func(realm, model string) string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	windowed := hours > 0
	if windowed && hours > 24*60 {
		hours = 24 * 60
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}
	creditAcctAgg := map[string]*creditAcc{}
	creditModelAgg := map[string]*creditAcc{}
	rateCache := map[string]string{}
	dailyAll := map[string]*aggAcc{}
	optModels := map[string]bool{}
	optUIDs := map[string]bool{}

	var hourFrom time.Time
	if windowed {
		nowHour := time.Now().Truncate(time.Hour)
		hourFrom = nowHour.Add(-time.Duration(hours-1) * time.Hour)
	}

	// 数据起点（全库最早分片）：不受窗口影响，表示"记录自何时开始"。scope 字典序
	// 即时间序（同前缀内同格式排序；"d:" 恒早于 "h:"——日桶只来自 90 天前的小时折叠）。
	since := ""
	matched := 0
	for i := range bs {
		b := &bs[i]
		if b.Scope < since || since == "" {
			since = b.Scope
		}
		// 下拉框选项先于口径过滤收集：全库有数据的维度都要能选，否则选中某个
		// 模型/账号后选项表只剩它自己，用户切不回去。
		if b.Model != "" {
			optModels[b.Model] = true
		}
		if b.UID != "" {
			optUIDs[b.UID] = true
		}
		if (f.Model != "" && b.Model != f.Model) || (f.UID != "" && b.UID != f.UID) {
			continue
		}
		// 按天聚合放在窗口过滤**之前**：Daily 是全量日序列，窗口由前端按整日
		// 口径裁剪。放在这里也顺手把「一个自然日被小时桶与日桶各占一半」的
		// 边界情形（Rollup 的 90 天切点）合并成一天。
		if dk := dayOf(b.Scope); dk != "" {
			if dailyAll[dk] == nil {
				dailyAll[dk] = &aggAcc{}
			}
			dailyAll[dk].add(b)
		}
		if windowed {
			var ts time.Time
			var err error
			if strings.HasPrefix(b.Scope, "h:") {
				ts, err = time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
			} else {
				ts, err = time.ParseInLocation(dayLayout, strings.TrimPrefix(b.Scope, "d:"), time.Local)
			}
			// 解析失败的脏桶不进窗口聚合（也不该出现在任何口径里）。
			if err != nil || ts.Before(hourFrom) {
				continue
			}
		}
		matched++
		total.add(b)

		if realmAgg[b.Realm] == nil {
			realmAgg[b.Realm] = &aggAcc{}
		}
		realmAgg[b.Realm].add(b)

		if acctAgg[b.UID] == nil {
			acctAgg[b.UID] = &aggAcc{}
		}
		acctAgg[b.UID].add(b)
		// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
		// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
		if acctRealm[b.UID] == "" {
			acctRealm[b.UID] = b.Realm
		}

		if modelAgg[b.Model] == nil {
			modelAgg[b.Model] = &aggAcc{}
		}
		modelAgg[b.Model].add(b)

		if strings.HasPrefix(b.Scope, "h:") {
			scope := strings.TrimPrefix(b.Scope, "h:")
			if hourSeries[scope] == nil {
				hourSeries[scope] = &aggAcc{}
			}
			hourSeries[scope].add(b)
		} else {
			scope := strings.TrimPrefix(b.Scope, "d:")
			if daySeries[scope] == nil {
				daySeries[scope] = &aggAcc{}
			}
			daySeries[scope].add(b)
		}
		if b.CRN > 0 {
			ca := creditAcctAgg[b.UID]
			if ca == nil {
				ca = &creditAcc{CreditAgg: CreditAgg{
					Key:      b.UID,
					Realm:    b.Realm,
					Nickname: nicks[b.UID],
				}}
				creditAcctAgg[b.UID] = ca
			}
			ca.add(b)

			model := canonicalUsageModel(b.Model)
			rate := b.Rate
			if rate == "" && currentRate != nil {
				cacheKey := b.Realm + "\x00" + model
				if cached, ok := rateCache[cacheKey]; ok {
					rate = cached
				} else {
					rate = currentRate(b.Realm, model)
					rateCache[cacheKey] = rate
				}
			}
			modelKey := model + "\x00" + rate
			cm := creditModelAgg[modelKey]
			if cm == nil {
				cm = &creditAcc{CreditAgg: CreditAgg{Key: model, Rate: rate}}
				creditModelAgg[modelKey] = cm
			}
			cm.add(b)
		}
	}

	snap := Snapshot{
		Totals: total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) {
			return k, ""
		}),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel:         keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		CreditByAccount: creditKeyed(creditAcctAgg),
		CreditByModel:   creditKeyed(creditModelAgg),
		Daily:           points(dailyAll, "day"),
		Models:          sortedKeys(optModels),
		Buckets:         matched,
		Generated:       time.Now().Format(time.RFC3339),
	}
	for _, uid := range sortedKeys(optUIDs) {
		snap.Accounts = append(snap.Accounts, AccountOption{UID: uid, Nickname: nicks[uid]})
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// 日点（升序）+ 小时点（升序）拼成一条连续时序。日点这里只有 90 天前折叠
	// 出的日桶（窗口内），当前窗口内的逐日用量看 Daily。
	snap.Series = append(points(daySeries, "day"), points(hourSeries, "hour")...)

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// since 去掉 scope 前缀（"h:2026-09-16T13" → "2026-09-16T13"）给前端展示；
	// 无任何桶时保持空（无数据不伪造起点）。
	snap.Since = strings.TrimPrefix(strings.TrimPrefix(since, "h:"), "d:")
	return snap
}

func canonicalUsageModel(model string) string {
	model = strings.TrimSpace(model)
	for _, prefix := range []string{"cn:", "global:"} {
		if strings.HasPrefix(model, prefix) {
			model = strings.TrimPrefix(model, prefix)
			break
		}
	}
	if model == "" {
		return "(unknown)"
	}
	return model
}

func creditKeyed(m map[string]*creditAcc) []CreditAgg {
	out := make([]CreditAgg, 0, len(m))
	for _, v := range m {
		out = append(out, v.finish())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Credits != out[j].Credits {
			return out[i].Credits > out[j].Credits
		}
		if out[i].CreditTokens != out[j].CreditTokens {
			return out[i].CreditTokens > out[j].CreditTokens
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Rate < out[j].Rate
	})
	return out
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
