package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// drainTestPool 构造指定到期/余额的池；exp 相对 now 的天数（-1 = 无到期观测）。
func drainTestPool(t *testing.T, exp map[string]int, credits map[string]int64) *Pool {
	t.Helper()
	p := New("")
	now := time.Now()
	for uid, c := range credits {
		a := &auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999}
		p.Add(a)
		p.SetCredits(uid, c, 0)
		if d, ok := exp[uid]; ok && d >= 0 {
			p.noteExpiryForTest(uid, now.AddDate(0, 0, d))
		}
	}
	return p
}

// noteExpiryForTest 直接写 creditsExpireAt（生产由签到/余额刷新设置，测试注入用）。
func (p *Pool) noteExpiryForTest(uid string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.creditsExpireAt = at
	}
}

// TestDrainStrategySerialExhaustion 串行耗尽策略：到期最早段优先；同段内余额最小者
// 做「排水头」且排序稳定不翻转；头部耗尽（硬冷却）退出候选后下一个才顶上；
// 无到期观测的号排最后。
func TestDrainStrategySerialExhaustion(t *testing.T) {
	p := drainTestPool(t,
		map[string]int{"early-big": 5, "early-small": 5, "later": 10, "no-exp": -1},
		map[string]int64{"early-big": 900, "early-small": 200, "later": 5000, "no-exp": 9999})
	p.SetSelection(SelectionDrain)
	if p.SelectionName() != SelectionDrain {
		t.Fatalf("SelectionName = %s, want drain", p.SelectionName())
	}

	// 第一选：到期最早的段（early-*，5 天后到期）里余额最小者。
	if got := p.Pick().UID; got != "early-small" {
		t.Fatalf("第一选 = %s, want early-small（最早段内余额最小者）", got)
	}
	// 排水头被消耗后余额仍最小（200→150），排序不翻转：还是它，不切号。
	p.SetCredits("early-small", 150, 0)
	if got := p.Pick().UID; got != "early-small" {
		t.Fatalf("头部消耗后 = %s, want early-small（排序稳定不切号）", got)
	}
	// 头部耗尽 → 硬冷却退出候选集 → 同段另一个号顶上（而不是跳到 later/no-exp）。
	p.Cooldown("early-small", CoolHard, 12*time.Hour, "余额不足（测试）")
	if got := p.Pick().UID; got != "early-big" {
		t.Fatalf("头部耗尽后 = %s, want early-big（同段接续，不跨段）", got)
	}
	// 整段耗尽 → 下一段（later）；无到期观测者排最后。
	p.Cooldown("early-big", CoolHard, 12*time.Hour, "余额不足（测试）")
	if got := p.Pick().UID; got != "later" {
		t.Fatalf("整段耗尽后 = %s, want later（下一段；no-exp 应排最后）", got)
	}
}

// TestDrainStrategyFallbackWhenAllCooling 全部冷却时仍走 pickEarliestExpiryLocked
// 兜底（drain 不改变兜底语义），避免串行策略把可用性打穿。
func TestDrainStrategyFallbackWhenAllCooling(t *testing.T) {
	p := drainTestPool(t,
		map[string]int{"a": 3},
		map[string]int64{"a": 100})
	p.SetSelection(SelectionDrain)
	p.CooldownSoftRate("a", 60*time.Second, time.Now().Add(time.Minute), "429")
	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("全冷却兜底应仍可选中 a, got %+v", got)
	}
}

// TestSetSelection 未知策略名被拒绝且保持原策略（配置校验的最后一道防线）。
func TestSetSelection(t *testing.T) {
	p := New("")
	if p.SelectionName() != SelectionWeighted {
		t.Fatalf("缺省策略 = %s, want weighted", p.SelectionName())
	}
	if p.SetSelection("bogus") {
		t.Fatal("未知策略名应返回 false")
	}
	if p.SelectionName() != SelectionWeighted {
		t.Fatalf("被拒后应保持原策略, got %s", p.SelectionName())
	}
	if !p.SetSelection(SelectionDrain) || p.SelectionName() != SelectionDrain {
		t.Fatal("应能切换到 drain")
	}
}

// TestRiskSignalPersisted 风控信号台账：终身累计 + 首现/最近，落盘→恢复不丢；
// 未出现过的号 Status.RiskSignals 为 nil（区分「干净」与「没统计」）。
func TestRiskSignalPersisted(t *testing.T) {
	path := t.TempDir() + "/state.json"
	p := New(path)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteRiskSignal("u1", "12153")
	p.NoteRiskSignal("u1", "12153")
	p.NoteRiskSignal("u1", "waf403")

	st, ok := p.Status("u1")
	if !ok || len(st.RiskSignals) != 2 {
		t.Fatalf("status risk = %+v, want 2 行", st.RiskSignals)
	}
	if st.RiskSignals[0].Code != "12153" || st.RiskSignals[0].Count != 2 {
		t.Fatalf("12153 行 = %+v, want count=2（code 稳定排序）", st.RiskSignals[0])
	}

	p.Flush() // 强制落盘
	p2 := New(path)
	st2, ok := p2.Status("u1")
	if !ok || len(st2.RiskSignals) != 2 || st2.RiskSignals[0].Count != 2 {
		t.Fatalf("恢复后 risk = %+v, want 12153×2 不丢", st2.RiskSignals)
	}
	if st2.RiskSignals[0].FirstSeen.IsZero() {
		t.Fatal("首现时刻应随落盘恢复")
	}

	p2.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999})
	if st3, _ := p2.Status("u2"); st3.RiskSignals != nil {
		t.Fatalf("未出现信号的号 RiskSignals 应为 nil, got %+v", st3.RiskSignals)
	}
}
