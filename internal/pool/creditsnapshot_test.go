package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// creditsView 曝露积分运行态供测试断言（包内私有 helper，与 breakerUntil 同模式）。
func (p *Pool) creditsView(uid string) (expiring int64, earliestAt time.Time, earliestRemaining int64, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, time.Time{}, 0, false
	}
	return e.creditsExpiring, e.creditsEarliestExpiry, e.creditsEarliestRemaining, true
}

func statusByUID(t *testing.T, p *Pool, uid string) Status {
	t.Helper()
	for _, st := range p.List() {
		if st.UID == uid {
			return st
		}
	}
	t.Fatalf("no status for %s", uid)
	return Status{}
}

// TestSetCreditsDetailedFields 快照字段经 Status 透出 + 「写数值」与「解冻」解耦：
// SetCreditsDetailed 只写观测值、不触碰冷却态——面板单号「余额」按钮因此天然是
// NoRevive 口径（不会把冷却/熔断中的号点亮）；「余额恢复 → 解冻」由
// ReenableIfCredits 承担（签到/全量刷新先 Reenable 再 Detailed）。
func TestSetCreditsDetailedFields(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "test-cool")
	exp := time.Now().Add(72 * time.Hour).Truncate(time.Second)

	// Detailed 只写观测值：冷却中照常刷新数值，不点亮也不解冻。
	p.SetCreditsDetailed("u1", 1000, 1200, 300, exp, 40)
	st := statusByUID(t, p, "u1")
	if st.Credits != 1000 || st.CreditsTotal != 1200 {
		t.Errorf("credits=%d/%d want 1000/1200", st.Credits, st.CreditsTotal)
	}
	if !st.CreditsEarliestExpiry.Equal(exp) {
		t.Errorf("CreditsEarliestExpiry=%v want %v", st.CreditsEarliestExpiry, exp)
	}
	if st.CreditsEarliestRemaining != 40 {
		t.Errorf("CreditsEarliestRemaining=%d want 40", st.CreditsEarliestRemaining)
	}
	if !st.Cooling {
		t.Errorf("SetCreditsDetailed must not touch cooling state (NoRevive 口径)")
	}
	// 解冻由 ReenableIfCredits 承担，且只清硬冷却（#55：不再误清软限流/6004 台账）；
	// 调度顺序 = 先 Reenable 再 Detailed（Detailed 会整体覆盖 expiring）。
	p.ReenableIfCredits("u1", 1000, 1200)
	p.SetCreditsDetailed("u1", 1000, 1200, 300, exp, 40)
	if st := statusByUID(t, p, "u1"); st.Cooling {
		t.Errorf("hard-cooling account with recovered balance should be reenabled")
	}
	if expiring, _, _, _ := p.creditsView("u1"); expiring != 300 {
		t.Errorf("creditsExpiring=%d want 300", expiring)
	}
}

// TestSetCreditsDetailedClampsAndClears 快过期子集与最早批次的钳制 + 无效批次清空。
func TestSetCreditsDetailedClampsAndClears(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	exp := time.Now().Add(time.Hour).Truncate(time.Second)

	// expiring / earliestRemaining 超过 credits → 钳到 credits；负值 → 0。
	p.SetCreditsDetailed("u1", 10, 100, 999, exp, 999)
	if got, _, rem, _ := p.creditsView("u1"); got != 10 || rem != 10 {
		t.Errorf("expiring/remaining=%d/%d want 10/10 (clamped to credits)", got, rem)
	}
	p.SetCreditsDetailed("u1", 10, 100, -5, exp, -5)
	if got, _, rem, _ := p.creditsView("u1"); got != 0 || rem != 0 {
		t.Errorf("negative must clamp to 0, got %d/%d", got, rem)
	}

	// earliestAt 零值 / 已过期 / 剩余 0 → 最早批次清空（不路由也不展示陈旧批次）。
	p.SetCreditsDetailed("u1", 10, 100, 5, time.Time{}, 5)
	if _, at, _, _ := p.creditsView("u1"); !at.IsZero() {
		t.Errorf("zero earliestAt must clear batch, got %v", at)
	}
	p.SetCreditsDetailed("u1", 10, 100, 5, time.Now().Add(-time.Hour), 5)
	if _, at, _, _ := p.creditsView("u1"); !at.IsZero() {
		t.Errorf("past earliestAt must clear batch, got %v", at)
	}
	p.SetCreditsDetailed("u1", 10, 100, 5, exp, 0)
	if _, at, _, _ := p.creditsView("u1"); !at.IsZero() {
		t.Errorf("zero remaining must clear batch, got %v", at)
	}
}

// TestCreditsEarliestPersistRoundtrip 最早到期批次跨重启保留，重启侧对无效
//（已过期/超量）数据惰性清洗，不复活「昨天到期」的陈旧批次。
func TestCreditsEarliestPersistRoundtrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 1200, 400, exp, 482)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"credits_earliest_expiry"`) {
		t.Fatalf("state.json 应含最早到期批次: %s", raw)
	}

	p2 := New(fp)
	p2.load()
	if _, at, rem, ok := p2.creditsView("u1"); !ok || !at.Equal(exp) || rem != 482 {
		t.Errorf("u1 restore: at=%v rem=%d (want %v/482)", at, rem, exp)
	}
	st := statusByUID(t, p2, "u1")
	if !st.CreditsEarliestExpiry.Equal(exp) || st.CreditsEarliestRemaining != 482 {
		t.Errorf("restored status: %v/%d want %v/482",
			st.CreditsEarliestExpiry, st.CreditsEarliestRemaining, exp)
	}
}
