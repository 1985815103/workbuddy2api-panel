package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// pinnedKey 绑定 u1 的用户 key；adminKey 是池管理员密钥。
const (
	pinnedKey = "sk-wb2api-user-bound-u1"
	adminKey  = "sk-wb2api-admin"
)

// newPinnedHandler 池内 u1（绑定号）+ u2（高余额号）；若 disableU1 则把 u1 禁用。
// APIUsers 走静态字段（测试无 Live），验证 loadLive 回退路径。
func newPinnedHandler(t *testing.T, disableU1 bool) *Handler {
	t.Helper()
	u1 := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	h := NewHandler(Config{
		Pool:     testPoolWith(u1, &auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   adminKey,
		APIUsers: map[string]livecfg.PinnedAccount{"sk-wb2api-user-bound-u1": {UID: "u1", Name: "九月"}},
	})
	h.cfg.Pool.SetCredits("u2", 9000, 0) // u2 权重远高于 u1：绑号路由必须无视权重
	if disableU1 {
		h.cfg.Pool.Disable("u1", "测试禁用")
	}
	return h
}

// chatWithKey 以指定 Bearer 发起一次流式聊天，返回响应码与 body。
func chatWithKey(h *Handler, key string) (int, string) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestPinnedKeyRoutesToBoundAccount 绑定 key 只走绑定号：即使池内另一号权重高得多
// （选号权重对绑号路由完全失效），上游必须被 u1 的凭证调用。
func TestPinnedKeyRoutesToBoundAccount(t *testing.T) {
	var usedAuthz string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		usedAuthz = authz
		return 200, sseOK, true
	})
	h := newPinnedHandler(t, false)
	h.cfg.Upstream = up

	code, body := chatWithKey(h, pinnedKey)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if !strings.Contains(usedAuthz, "at1") {
		t.Fatalf("上游被调用的凭证 = %q, want u1 的 at1（绑号路由）", usedAuthz)
	}
	// /status 对绑定 key 关闭（全池余额/冷却不给绑定方看）。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+pinnedKey)
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("绑定 key 读 /status = %d, want 403", rec.Code)
	}
	// admin key 一切照旧。
	if code, _ := chatWithKey(h, adminKey); code != 200 {
		t.Fatalf("admin key chat code=%d", code)
	}
}

// TestPinnedKeyNeverRotates 绑定号不可用（禁用）时报 bound_account_unavailable，
// 绝不轮换到池内其他号——一换号，「谁的 key 记谁的账」即告破灭。
func TestPinnedKeyNeverRotates(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := newPinnedHandler(t, true)
	h.cfg.Upstream = up

	code, body := chatWithKey(h, pinnedKey)
	if code != 503 || !strings.Contains(body, "bound_account_unavailable") {
		t.Fatalf("code=%d body=%s, want 503 bound_account_unavailable", code, body)
	}
	if calls != 0 {
		t.Fatalf("上游被调用 %d 次, want 0（绑定号不可用时不得改打其他号）", calls)
	}
}

// TestApplyErrorPolicyRecordsRiskSignals 风控台账接线：各错误分类分支喂入对应
// 错误码，Status.RiskSignals 可读出（次数累计 + code 稳定排序）。
func TestApplyErrorPolicyRecordsRiskSignals(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})})

	h.applyErrorPolicy("u1", upstream.ErrHardCredit, "", "glm-5.2", nil)
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, "", "glm-5.2", nil)
	h.applyErrorPolicy("u1", upstream.ErrWafBlock, "", "glm-5.2", nil)

	st, ok := h.cfg.Pool.Status("u1")
	if !ok || len(st.RiskSignals) != 3 {
		t.Fatalf("risk = %+v, want 14018/12153/waf403 三行", st.RiskSignals)
	}
	want := map[string]int64{"12153": 1, "14018": 1, "waf403": 1}
	for _, r := range st.RiskSignals {
		if want[r.Code] != r.Count {
			t.Fatalf("code %s count=%d, want %d", r.Code, r.Count, want[r.Code])
		}
	}
}
