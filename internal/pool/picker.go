package pool

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

var ErrNoEligibleAccounts = errors.New("no eligible accounts")

type Candidate struct {
	ID               string
	Token            string
	Email            string
	UserID           string
	TeamID           string
	ExpiresAt        *time.Time
	Enabled          bool
	DisabledForQuota bool
	CooldownUntil    *time.Time
	BlockedModels    map[string]any
	RequestCount     int64
	Weight           int
}

func (c Candidate) Eligible(model string, now time.Time) bool {
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Token) == "" {
		return false
	}
	if !c.Enabled || c.DisabledForQuota {
		return false
	}
	if c.ExpiresAt != nil && !c.ExpiresAt.After(now) {
		return false
	}
	if c.CooldownUntil != nil && c.CooldownUntil.After(now) {
		return false
	}
	return !modelBlocked(c.BlockedModels, model, now)
}

func (c Candidate) UpstreamAccount() grok.Account {
	return grok.Account{ID: c.ID, Token: c.Token}
}

func Pick(candidates []Candidate, model, mode string, now time.Time) (Candidate, error) {
	chain := Chain(candidates, model, mode, now, 1)
	if len(chain) == 0 {
		return Candidate{}, ErrNoEligibleAccounts
	}
	return chain[0], nil
}

func Chain(candidates []Candidate, model, mode string, now time.Time, max int) []Candidate {
	eligible := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Eligible(model, now) {
			eligible = append(eligible, candidate)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	sortCandidates(eligible, strings.ToLower(strings.TrimSpace(mode)))
	if max <= 0 || max > len(eligible) {
		max = len(eligible)
	}
	return append([]Candidate(nil), eligible[:max]...)
}

func sortCandidates(candidates []Candidate, mode string) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		switch mode {
		case "least_used":
			if a.RequestCount != b.RequestCount {
				return a.RequestCount < b.RequestCount
			}
		case "random":
			// Keep Go migration deterministic until parity fixtures cover random mode.
			if a.Weight != b.Weight {
				return a.Weight > b.Weight
			}
		default: // round_robin initial read-only foundation: stable weighted order.
			if a.Weight != b.Weight {
				return a.Weight > b.Weight
			}
		}
		return a.ID < b.ID
	})
}

func modelBlocked(blocked map[string]any, model string, now time.Time) bool {
	model = strings.TrimSpace(model)
	if model == "" || len(blocked) == 0 {
		return false
	}
	value, ok := blocked[model]
	if !ok {
		value, ok = blocked[strings.ToLower(model)]
	}
	if !ok || value == nil {
		return false
	}
	nowUnix := float64(now.Unix())
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return numericBlockActive(v, nowUnix)
	case float32:
		return numericBlockActive(float64(v), nowUnix)
	case int64:
		return numericBlockActive(float64(v), nowUnix)
	case int:
		return numericBlockActive(float64(v), nowUnix)
	case string:
		s := strings.TrimSpace(v)
		if s == "" || s == "0" || strings.EqualFold(s, "false") {
			return false
		}
		return true
	case map[string]any:
		if b, ok := v["blocked"].(bool); ok && !b {
			return false
		}
		if until, ok := v["until"]; ok && until != nil {
			var u float64
			switch x := until.(type) {
			case float64:
				u = x
			case float32:
				u = float64(x)
			case int:
				u = float64(x)
			case int64:
				u = float64(x)
			case string:
				// ignore parse errors -> treat permanent
			}
			if u > 1e12 {
				u = u / 1000
			}
			if u > 0 {
				return u > nowUnix
			}
		}
		return true
	default:
		return true
	}
}

func numericBlockActive(v, nowUnix float64) bool {
	if v <= 0 {
		return true // permanent marker
	}
	u := v
	if u > 1e12 {
		u = u / 1000
	}
	// unix timestamp after 2020-01-01 => until
	if u > 1577836800 {
		return u > nowUnix
	}
	// small numbers like 1 => permanent true
	return true
}
