package balancer

import (
	"sort"
	"time"
)

type codexQuotaCandidate struct {
	id    string
	score float64
}

// PickCodexSession preserves existing bindings before considering quota
// calibration. Any account selected for calibration owns the new conversation.
func (b *Balancer) PickCodexSession(keyHash, sessionID, parentID string, candidates []Candidate, cfg Config, resolver QuotaResolver) (string, bool) {
	if sessionID == "" {
		// Let CPA apply its own session-affinity fallback for unidentified clients.
		return "", false
	}
	cfg = cfg.WithDefaults()
	now := b.now()
	eligible := make([]Candidate, 0, len(candidates))
	ranked := make([]codexQuotaCandidate, 0, len(candidates))
	best := -1.0
	for _, candidate := range candidates {
		if candidate.ID == "" {
			continue
		}
		var info QuotaInfo
		if resolver != nil {
			info = resolver.Lookup(candidate.ID, candidate.Provider)
		}
		score := codexQuotaScore(info, now)
		if info.BlockedUntil.After(now) || score == 0 {
			continue
		}
		eligible = append(eligible, candidate)
		ranked = append(ranked, codexQuotaCandidate{id: candidate.ID, score: score})
		best = max(best, score)
	}
	if len(eligible) == 0 {
		return "", false
	}

	// Namespace by caller to keep unrelated clients with the same session ID apart.
	sessionKey := "codex-session:" + keyHash + ":" + sessionID
	parentKey := "codex-session:" + keyHash + ":" + parentID
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now, cfg)

	if entry, ok := b.sticky[sessionKey]; ok {
		if containsCandidate(eligible, entry.authID) {
			b.sticky[sessionKey] = stickyEntry{authID: entry.authID, lastUsed: now}
			return entry.authID, true
		}
	} else if parent, ok := b.sticky[parentKey]; ok && parentID != "" && containsCandidate(eligible, parent.authID) {
		b.sticky[sessionKey] = stickyEntry{authID: parent.authID, lastUsed: now}
		return parent.authID, true
	}

	chosen, calibrate := calibrationTarget(b.divert, keyHash, eligible, now)
	if calibrate {
		b.divert.Pending.Attempted(chosen)
	} else {
		// Rotate among effectively tied profiles to avoid concentrating
		// concurrent new conversations before quota observations catch up.
		const relativeTie = 0.90
		var choices []string
		for _, candidate := range ranked {
			if best < 0 || candidate.score >= best*relativeTie {
				choices = append(choices, candidate.id)
			}
		}
		sort.Strings(choices)
		chosen = choices[0]
		if next := sort.Search(len(choices), func(i int) bool { return choices[i] > b.lastCodexAuth }); next < len(choices) {
			chosen = choices[next]
		}
	}
	b.lastCodexAuth = chosen
	b.sticky[sessionKey] = stickyEntry{authID: chosen, lastUsed: now}
	return chosen, true
}

// codexQuotaScore measures percentage points of quota available per hour
// before reset. Unknown or stale measurements get no rank.
func codexQuotaScore(info QuotaInfo, now time.Time) float64 {
	if info.LongWindowStale || info.UsedPercent == nil || !info.LongResetAt.After(now) {
		return -1
	}
	remaining := 100 - *info.UsedPercent
	if !(remaining >= 0 && remaining <= 100) {
		return -1
	}
	hours := max(1, info.LongResetAt.Sub(now).Hours())
	return remaining / hours
}
