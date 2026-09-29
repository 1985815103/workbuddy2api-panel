package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// captureStdout 重定向 os.Stdout（连同 chatLogOut，见 SetChatLogOutput 的注入点）
// 并捕获 fn 期间的全部输出。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	oldOut := chatLogOut
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	chatLogOut = w
	fn()
	os.Stdout = old
	chatLogOut = oldOut
	_ = w.Close()
	raw, _ := io.ReadAll(r)
	return string(raw)
}

// withChatLog 临时开启聊天表格日志（TestMain 默认关闭），测试结束后恢复。
// 仅供断言表格行输出的用例使用。
func withChatLog(t *testing.T) {
	t.Helper()
	old := chatLogEnabled
	chatLogEnabled = true
	t.Cleanup(func() { chatLogEnabled = old })
}

func TestChatStatsReaderTokensFromUsage(t *testing.T) {
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now())
	// 保证 TTFB 跨过时钟粒度：Windows 上纯内存读取不足 1ms，time.Since 可能取 0。
	time.Sleep(3 * time.Millisecond)
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	toks, ok := r.Tokens()
	if !ok || toks != 1 {
		t.Fatalf("tokens=%d ok=%v, want 1/true (from usage, not rune count)", toks, ok)
	}
	usage := r.Usage()
	if !usage.HasPromptTokens || usage.PromptTokens != 1 || !usage.HasCompletionTokens || usage.CompletionTokens != 1 || !usage.HasTotalTokens || usage.TotalTokens != 2 {
		t.Fatalf("usage=%+v, want prompt=1 completion=1 total=2", usage)
	}
	if r.TTFB() <= 0 {
		t.Errorf("ttfb=%v want >0", r.TTFB())
	}
}

func TestChatStatsReaderNoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); ok || toks != 0 {
		t.Errorf("tokens=%d ok=%v, want 0/false for missing usage", toks, ok)
	}
}

// 流式路径：末帧 usage.credit 进 Usage()，供用量记录器累计积分消耗。
func TestChatStatsReaderCreditFromUsage(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14,\"credit\":1.25}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	usage := r.Usage()
	if !usage.HasCredit || usage.Credit != 1.25 {
		t.Fatalf("credit=%v ok=%v, want 1.25/true", usage.Credit, usage.HasCredit)
	}
	// 免费请求 credit=0：是观测值，HasCredit 必须为 true（区分「全免费」与「未上报」）。
	free := "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"credit\":0}}\n\ndata: [DONE]\n\n"
	r2 := newChatStatsReaderSince(strings.NewReader(free), time.Now())
	_, _ = io.Copy(io.Discard, r2)
	if u := r2.Usage(); !u.HasCredit || u.Credit != 0 {
		t.Fatalf("free credit=%v ok=%v, want 0/true", u.Credit, u.HasCredit)
	}
}

// 非流式路径：usageDeltaFromResponse 提取 usage.credit；缺失时不置 HasCredit。
func TestUsageDeltaFromResponseCredit(t *testing.T) {
	d := usageDeltaFromResponse(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0, "total_tokens": 15.0, "credit": 2.5},
	})
	if !d.HasCredit || d.Credit != 2.5 {
		t.Fatalf("credit=%v ok=%v, want 2.5/true", d.Credit, d.HasCredit)
	}
	d2 := usageDeltaFromResponse(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0, "total_tokens": 15.0},
	})
	if d2.HasCredit {
		t.Fatalf("未上报 credit 时 HasCredit 应为 false: %+v", d2)
	}
}

// 流式路径：末帧 usage 的前缀缓存口径进 Usage()——面板拆「积分 ÷ 合计 token」的依据。
func TestChatStatsReaderCacheTokensFromUsage(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":4,\"total_tokens\":104," +
		"\"prompt_cache_hit_tokens\":90,\"prompt_cache_miss_tokens\":10}}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	u := r.Usage()
	if !u.HasCacheTokens || u.CacheHitTokens != 90 || u.CacheMissTokens != 10 {
		t.Fatalf("cache=%+v, want hit=90 miss=10 has=true", u)
	}

	// 只报一半（成对约定被破坏）→ 按未上报处理：把「没报 miss」读成「miss=0」
	// 会凭空造出一个 100% 命中率，比读成"无数据"危险得多。
	partial := "data: {\"usage\":{\"prompt_tokens\":100,\"prompt_cache_hit_tokens\":90}}\n\ndata: [DONE]\n\n"
	r2 := newChatStatsReaderSince(strings.NewReader(partial), time.Now())
	_, _ = io.Copy(io.Discard, r2)
	if u2 := r2.Usage(); u2.HasCacheTokens {
		t.Fatalf("只报 hit 时不应采信: %+v", u2)
	}
}

// 非流式路径：usageDeltaFromResponse 同样提取前缀缓存口径，且成对才采信。
func TestUsageDeltaFromResponseCache(t *testing.T) {
	d := usageDeltaFromResponse(map[string]any{
		"usage": map[string]any{
			"prompt_tokens": 10.0, "prompt_cache_hit_tokens": 8.0, "prompt_cache_miss_tokens": 2.0,
		},
	})
	if !d.HasCacheTokens || d.CacheHitTokens != 8 || d.CacheMissTokens != 2 {
		t.Fatalf("cache=%+v, want hit=8 miss=2 has=true", d)
	}
	d2 := usageDeltaFromResponse(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "prompt_cache_hit_tokens": 8.0},
	})
	if d2.HasCacheTokens {
		t.Fatalf("缺 miss 字段时 HasCacheTokens 应为 false: %+v", d2)
	}
}

// 端到端透传：SSE 末帧的缓存口径经 chatStatsReader → usage.Delta → usage.Recorder
// 落到分桶。中间任何一环丢字段都只表现为面板「命中率」恒空，单看解析层测不出来。
func TestCacheTokensReachUsageRecorder(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
			"\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":6,\"total_tokens\":126," +
			"\"prompt_cache_hit_tokens\":100,\"prompt_cache_miss_tokens\":20,\"credit\":1.5}}\n\n" +
			"data: [DONE]\n\n", true
	})
	rec := usage.New("")
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Usage:    rec,
	})
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[]}`)))
	if rec3.Code != 200 {
		t.Fatalf("code=%d body=%s", rec3.Code, rec3.Body)
	}

	s := rec.Snapshot(24, nil)
	if s.Totals.CacheHitTokens != 100 || s.Totals.CacheMissTokens != 20 || s.Totals.CacheSamples != 1 {
		t.Fatalf("cache 未落桶: %d/%d/%d, want 100/20/1",
			s.Totals.CacheHitTokens, s.Totals.CacheMissTokens, s.Totals.CacheSamples)
	}
	if s.Totals.CacheHitRate <= 0.833 || s.Totals.CacheHitRate >= 0.834 {
		t.Fatalf("hit rate = %v, want 100/120", s.Totals.CacheHitRate)
	}
	if s.Totals.Credits != 1.5 || s.Totals.TotalTokens != 126 {
		t.Fatalf("同请求的积分/token 口径被带偏: credits=%v total=%d", s.Totals.Credits, s.Totals.TotalTokens)
	}
}

func TestChatStatsReaderLastFrameUsageWins(t *testing.T) {
	sse := "data: {\"usage\":{\"completion_tokens\":5}}\n\n" +
		"data: {\"usage\":{\"completion_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	toks, ok := r.Tokens()
	if !ok || toks != 12 {
		t.Fatalf("tokens=%d ok=%v, want 12 (last frame wins)", toks, ok)
	}
}

func TestChatStatsReaderTTFBOnlyOnDataFrame(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	var s chatStatsReader
	s.start = start
	s.parseSSELine("event: ping")
	if s.TTFB() != 0 {
		t.Errorf("non-data line must not set TTFB: %v", s.TTFB())
	}
	s.parseSSELine("data: {\"choices\":[]}")
	first := s.TTFB()
	if first < time.Second {
		t.Errorf("first data frame TTFB=%v want >=2s", first)
	}
	s.parseSSELine("data: {\"choices\":[]}")
	if s.TTFB() != first {
		t.Errorf("second frame changed TTFB: %v -> %v", first, s.TTFB())
	}
}

func TestChatStatsReaderBytesPassthrough(t *testing.T) {
	sse := "data: {\"content\":\"你好\"}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	out, _ := io.ReadAll(r)
	if string(out) != sse {
		t.Errorf("passthrough mismatch:\n got %q\nwant %q", out, sse)
	}
}

func TestParseModelFromBody(t *testing.T) {
	if got := parseModelFromBody([]byte(`{"model":"deepseek-v4-flash","stream":true}`)); got != "deepseek-v4-flash" {
		t.Errorf("got %q", got)
	}
	if got := parseModelFromBody([]byte(`{}`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
	if got := parseModelFromBody([]byte(`not json`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
}

func TestCompletionTokensExtraction(t *testing.T) {
	got := completionTokens(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 234.0, "total_tokens": 244.0},
	})
	if got != 234 {
		t.Errorf("got %d want 234", got)
	}
	if got := completionTokens(map[string]any{}); got != -1 {
		t.Errorf("missing usage: got %d want -1", got)
	}
	if got := completionTokens(map[string]any{"usage": map[string]any{}}); got != -1 {
		t.Errorf("missing completion_tokens: got %d want -1", got)
	}
}

func TestUIDPrefix(t *testing.T) {
	if got := uidPrefix("00e26541abcdef012345"); got != "00e26541" {
		t.Errorf("long uid -> %q", got)
	}
	if got := uidPrefix("abc"); got != "abc" {
		t.Errorf("short uid -> %q", got)
	}
	if got := uidPrefix(""); got != "-" {
		t.Errorf("empty uid -> %q", got)
	}
}

func TestLogChatRowFormat(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(412*time.Millisecond, 27100*time.Millisecond, "deepseek-v4-flash", "stream", "00e26541abcdef", "示例号", http.StatusOK, 1234)
	})
	for _, want := range []string{
		"| #", "deepseek-v4", "| stream |", "| 200 |", "示例号(00e26541)", "TTFB=412ms", "tok=1234", "tok/s   |", "total=",
		// 速率 = 1234 ÷ (27.1s − 0.412s) = 46.2tok/s（生成窗口口径；旧总时长口径是 45.5）。
		"46.2tok/s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "00e26541abcdef") {
		t.Errorf("full uid leaked: %s", out)
	}
}

// TestLogChatRowRateExcludesTTFB 锁定「tok/s 是解码速度」语义：TTFB 占大头时，
// 分母必须扣掉首 token 延迟（100tok ÷ 4s 生成窗口 = 25.0tok/s），而不是摊进总时长
// （100 ÷ 10s = 10.0tok/s）——后者正是「短回复速率虚低一个数量级」误导的来源。
func TestLogChatRowRateExcludesTTFB(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(6*time.Second, 10*time.Second, "deepseek-v4.1-flash", "stream", "u1", "", http.StatusOK, 100)
	})
	if !strings.Contains(out, "25.0tok/s") {
		t.Errorf("want decode rate 25.0tok/s (100tok / 4s gen window):\n%s", out)
	}
	if strings.Contains(out, "10.0tok/s") {
		t.Errorf("rate must not be diluted by TTFB:\n%s", out)
	}
}

// TestLogChatRowRateGuard 锁定可信度护栏：生成窗口 < 500ms（整包倒出，TTFB≈total）
// 或 toks < 30 的行速率列显示 "-"，绝不输出小分母假象（如 39tok/26ms=1500tok/s）。
func TestLogChatRowRateGuard(t *testing.T) {
	withChatLog(t)
	// 整包倒出：TTFB=11289ms、total=11.3s → 生成窗口仅 ~26ms（1502tok/s 假象来源）。
	out := captureStdout(t, func() {
		logChatRow(11289*time.Millisecond, 11315*time.Millisecond, "cn:glm-5.3", "stream", "u1", "", http.StatusOK, 39)
	})
	if !strings.Contains(out, "tok=39") {
		t.Errorf("toks should still show:\n%s", out)
	}
	if strings.Contains(out, "tok/s") {
		t.Errorf("rate must be suppressed for tiny window:\n%s", out)
	}
	// 样本过少：10tok / 2s 窗口足够大但 toks < 30，同样不出数。
	out = captureStdout(t, func() {
		logChatRow(time.Second, 3*time.Second, "cn:glm-5.3", "stream", "u1", "", http.StatusOK, 10)
	})
	if !strings.Contains(out, "tok=10") {
		t.Errorf("toks should still show:\n%s", out)
	}
	if strings.Contains(out, "tok/s") {
		t.Errorf("rate must be suppressed for tiny token sample:\n%s", out)
	}
	// 边界之上正常出数：31tok / 1s 窗口 → 31.0tok/s。
	out = captureStdout(t, func() {
		logChatRow(time.Second, 2*time.Second, "cn:glm-5.3", "stream", "u1", "", http.StatusOK, 31)
	})
	if !strings.Contains(out, "31.0tok/s") {
		t.Errorf("want 31.0tok/s above guard thresholds:\n%s", out)
	}
}

func TestLogChatRowNoUsageShowsDash(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "glm-5.2", "sync", "s1", "", http.StatusServiceUnavailable, -1)
	})
	for _, want := range []string{"TTFB=-", "tok=-", "tok=-      |", "| 503 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
}

func TestLogChatRowSeqIncrements(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "sync", "u", "", 200, 1)
		logChatRow(0, time.Second, "m", "sync", "u", "", 200, 1)
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	first := strings.Fields(lines[0])[1]
	second := strings.Fields(lines[1])[1]
	if !strings.HasPrefix(first, "#") || !strings.HasPrefix(second, "#") {
		t.Fatalf("seq columns missing: %q %q", first, second)
	}
	if first == second {
		t.Errorf("seq not incremented: %q == %q", first, second)
	}
}

func TestChatLogsStreamRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| stream |", "| 200 |", "| u1 ", "TTFB=", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream row missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "tok=1") {
		t.Errorf("tok: want precise usage completion_tokens: %s", out)
	}
}

func TestChatLogsSyncRowTTFBDash(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| sync |", "| 200 |", "TTFB=-", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync row missing %q:\n%s", want, out)
		}
	}
}

func TestChatLogsErrorRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	for _, want := range []string{"| u1 ", "| 503 |", "tok=-"} {
		if !strings.Contains(out, want) {
			t.Errorf("error row missing %q:\n%s", want, out)
		}
	}
}

func TestHealthzDoesNotLogTableRow(t *testing.T) {
	withChatLog(t) // 日志开启也应无表格行：非 chat 路由根本不走 logChatRow
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models", nil))
		rec3 := httptest.NewRecorder()
		h.ServeHTTP(rec3, httptest.NewRequest("GET", "/status", nil))
	})
	if strings.Contains(out, "| #") {
		t.Errorf("healthz/models/status must not emit table rows:\n%s", out)
	}
}
