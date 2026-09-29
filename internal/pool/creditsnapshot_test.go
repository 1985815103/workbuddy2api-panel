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
func (p *Pool) creditsView(uid string) (expiring int64, expireAt time.Time, expireAmount int64, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, time.Time{}, 0, false
	}
	return e.creditsExpiring, e.creditsExpireAt, e.creditsExpireAmount, true
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

// TestSetCreditsSnapshotFieldsAndRevive 快照写入的字段透出 + 余额恢复解冻语义。
func TestSetCreditsSnapshotFieldsAndRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Minute, "test-cool")
	exp := time.Now().Add(72 * time.Hour).Truncate(time.Second)

	p.SetCreditsSnapshot("u1", CreditsSnapshot{
		Remain: 1000, Total: 1200, Expiring: 300, ExpireAt: exp, ExpireAmount: 40,
	})

	st := statusByUID(t, p, "u1")
	if st.Credits != 1000 || st.CreditsTotal != 1200 {
		t.Errorf("credits=%d/%d want 1000/1200", st.Credits, st.CreditsTotal)
	}
	if !st.CreditsExpireAt.Equal(exp) {
		t.Errorf("CreditsExpireAt=%v want %v", st.CreditsExpireAt, exp)
	}
	if st.CreditsExpireAmount != 40 {
		t.Errorf("CreditsExpireAmount=%d want 40", st.CreditsExpireAmount)
	}
	// 余额 > 0 → 自动解冻（与 ReenableIfCredits 同语义；不看是否有快过期积分）。
	if st.Cooling {
		t.Errorf("balance recovered, account should be revived (cooling still set)")
	}
	if expiring, _, _, _ := p.creditsView("u1"); expiring != 300 {
		t.Errorf("creditsExpiring=%d want 300", expiring)
	}
}

// TestSetCreditsSnapshotNoRevive 面板「余额」按钮口径：只刷数值，不点亮冷却中的号。
func TestSetCreditsSnapshotNoRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Minute, "test-cool")

	p.SetCreditsSnapshot("u1", CreditsSnapshot{Remain: 500, Total: 900, Expiring: 10, NoRevive: true})

	st := statusByUID(t, p, "u1")
	if !st.Cooling {
		t.Errorf("NoRevive must not clear cooling")
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500 (数值仍要刷新)", st.Credits)
	}
}

// TestSetCreditsSnapshotClampsAndClears 快过期子集钳制 + 余额归零清展示态。
func TestSetCreditsSnapshotClampsAndClears(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	exp := time.Now().Add(time.Hour).Truncate(time.Second)

	// expiring 超过 remain → 钳到 remain；负值 → 0。
	p.SetCreditsSnapshot("u1", CreditsSnapshot{Remain: 10, Total: 100, Expiring: 999})
	if got, _, _, _ := p.creditsView("u1"); got != 10 {
		t.Errorf("creditsExpiring=%d want 10 (clamped to remain)", got)
	}
	p.SetCreditsSnapshot("u1", CreditsSnapshot{Remain: 10, Total: 100, Expiring: -5})
	if got, _, _, _ := p.creditsView("u1"); got != 0 {
		t.Errorf("creditsExpiring=%d want 0 (negative clamped)", got)
	}

	// 余额归零 → 到期展示态清空（不显示「0 分 / 某日到期」）。
	p.SetCreditsSnapshot("u1", CreditsSnapshot{Remain: 0, Total: 100, ExpireAt: exp, ExpireAmount: 40})
	if _, at, amt, _ := p.creditsView("u1"); !at.IsZero() || amt != 0 {
		t.Errorf("zero balance should clear expiry display, got at=%v amt=%d", at, amt)
	}
	st := statusByUID(t, p, "u1")
	if !st.CreditsExpireAt.IsZero() || st.CreditsExpireAmount != 0 {
		t.Errorf("status should expose cleared expiry: %v/%d", st.CreditsExpireAt, st.CreditsExpireAmount)
	}
}

// TestCreditsExpirePersistRoundtrip 最近到期展示态跨重启保留；已过期的到期时刻不复活。
func TestCreditsExpirePersistRoundtrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCreditsSnapshot("u1", CreditsSnapshot{Remain: 1000, Total: 1200, Expiring: 400, ExpireAt: exp, ExpireAmount: 482})
	// u2：到期时刻已过 → 落盘侧惰性过滤，重启不该复活一个「昨天到期」的陈旧日期。
	p.SetCreditsSnapshot("u2", CreditsSnapshot{Remain: 5, Total: 10, ExpireAt: time.Now().Add(-time.Hour), ExpireAmount: 5})
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"credits_expire_at"`) || !strings.Contains(string(raw), `"credits_expire_amount": 482`) {
		t.Fatalf("state.json 应含到期展示态: %s", raw)
	}

	p2 := New(fp)
	p2.load()
	if _, at, amt, ok := p2.creditsView("u1"); !ok || !at.Equal(exp) || amt != 482 {
		t.Errorf("u1 restore: at=%v amt=%d (want %v/482)", at, amt, exp)
	}
	if _, at, amt, _ := p2.creditsView("u2"); !at.IsZero() || amt != 0 {
		t.Errorf("expired expiry must not be restored: at=%v amt=%d", at, amt)
	}
	st := statusByUID(t, p2, "u1")
	if !st.CreditsExpireAt.Equal(exp) {
		t.Errorf("restored status should carry expiry: %v", st.CreditsExpireAt)
	}
}
