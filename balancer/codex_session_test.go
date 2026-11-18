package balancer

import (
	"testing"
	"time"
)

func TestCodexSessionsRotateAndStayPinned(t *testing.T) {
	b := New()
	cfg := DefaultConfig()
	candidates := []Candidate{{ID: "b", Provider: "codex"}, {ID: "a", Provider: "codex"}}

	for _, check := range []struct {
		session, parent, want string
	}{
		{"first", "", "a"},
		{"second", "", "b"},
		{"first", "", "a"},
		{"second", "", "b"},
		{"child", "first", "a"},
		{"third", "", "a"},
		{"fourth", "", "b"},
	} {
		got, ok := b.PickCodexSession("one-key", check.session, check.parent, candidates, cfg, nil)
		if !ok || got != check.want {
			t.Fatalf("session %q got (%q, %t), want %q", check.session, got, ok, check.want)
		}
	}

	got, ok := b.PickCodexSession("one-key", "first", "", candidates[:1], cfg, nil)
	if !ok || got != "b" {
		t.Fatalf("expected failover to b, got (%q, %t)", got, ok)
	}
	got, ok = b.PickCodexSession("one-key", "first", "", candidates, cfg, nil)
	if !ok || got != "b" {
		t.Fatalf("expected binding to stay on b after failover, got (%q, %t)", got, ok)
	}
}

func TestCodexCalibrationWaitsForNewConversation(t *testing.T) {
	b := New()
	divert := &DivertState{Stats: NewKeyStats(), Pending: NewPendingCalibrations()}
	b.SetDivertState(divert)
	cfg := DefaultConfig()
	candidates := []Candidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}, {ID: "c", Provider: "codex"}}
	pick := func(id, want string) {
		t.Helper()
		if got, ok := b.PickCodexSession("one-key", id, "", candidates, cfg, nil); !ok || got != want {
			t.Fatalf("session %q got (%q, %t), want %q", id, got, ok, want)
		}
	}

	pick("existing", "a")
	divert.Pending.Mark("c")
	divert.Pending.marks["c"].markedAt = time.Now().Add(-defaultCalibrationTimeout - time.Minute)
	pick("existing", "a")
	pick("new", "c")
	pick("new", "c")
	pick("another", "a")
}

type codexTestQuota map[string]QuotaInfo

func (q codexTestQuota) Lookup(authID, _ string) QuotaInfo {
	return q[authID]
}

func TestCodexWeeklyQuotaRanksOnlyNewConversations(t *testing.T) {
	now := time.Now()
	b := newWithClock(func() time.Time { return now })
	cfg := DefaultConfig()
	candidates := []Candidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	weekly := func(used float64, until time.Duration) QuotaInfo {
		return QuotaInfo{UsedPercent: &used, LongResetAt: now.Add(until)}
	}
	quota := codexTestQuota{"a": weekly(66, 58*time.Hour), "b": weekly(15, 62*time.Hour)}
	pick := func(id, want string) {
		t.Helper()
		if got, ok := b.PickCodexSession("one-key", id, "", candidates, cfg, quota); !ok || got != want {
			t.Fatalf("session %q got (%q, %t), want %q", id, got, ok, want)
		}
	}

	pick("first", "b") // More weekly quota at a comparable reset time.
	quota["a"] = weekly(66, 12*time.Hour)
	pick("second", "a") // Less weekly quota, but it will reset much sooner.
	pick("first", "b")  // A rank change does not move an existing conversation.

	quota["a"], quota["b"] = weekly(30, 60*time.Hour), weekly(34, 60*time.Hour)
	pick("third", "b") // Near-tied scores rotate from the last new binding.
	pick("fourth", "a")

	quota["b"] = weekly(100, 60*time.Hour)
	pick("third", "a") // An exhausted weekly window permits failover.
}
