package auth

import (
	"net"
	"sync"
	"time"
)

// banRule defines a sliding window ban policy.
type banRule struct {
	window    time.Duration
	threshold int
	banDur    time.Duration
}

var banRules = []banRule{
	{window: 1 * time.Minute, threshold: 5, banDur: 1 * time.Minute},
	{window: 3 * time.Minute, threshold: 10, banDur: 5 * time.Minute},
}

const maxBanDuration = 10 * time.Minute

type ipState struct {
	mu         sync.Mutex
	failTimes  []time.Time     // timestamps of unique credential failures
	seenCreds  map[string]bool // deduplicate same user:pass failures
	bannedUtil time.Time
}

// Guard tracks per-IP failure counts and enforces banning.
type Guard struct {
	mu     sync.Mutex
	states map[string]*ipState
}

func NewGuard() *Guard {
	g := &Guard{states: make(map[string]*ipState)}
	go g.gc()
	return g
}

// IsBanned returns true if the IP is currently banned.
func (g *Guard) IsBanned(ip string) bool {
	s := g.getState(ip)
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.bannedUtil)
}

// RecordFailure records a credential failure for an IP.
// credKey should be "user:pass". Returns true if the IP is now banned.
func (g *Guard) RecordFailure(ip, credKey string) bool {
	s := g.getState(ip)
	s.mu.Lock()
	defer s.mu.Unlock()

	// already banned
	if time.Now().Before(s.bannedUtil) {
		return true
	}

	// deduplicate: same credential from same IP only counted once
	if s.seenCreds[credKey] {
		return false
	}
	s.seenCreds[credKey] = true
	s.failTimes = append(s.failTimes, time.Now())

	// evaluate ban rules (largest window first for stronger penalty)
	banDur := time.Duration(0)
	now := time.Now()
	for _, rule := range banRules {
		windowStart := now.Add(-rule.window)
		count := 0
		for _, t := range s.failTimes {
			if t.After(windowStart) {
				count++
			}
		}
		if count >= rule.threshold && rule.banDur > banDur {
			banDur = rule.banDur
		}
	}

	if banDur > 0 {
		if banDur > maxBanDuration {
			banDur = maxBanDuration
		}
		s.bannedUtil = now.Add(banDur)
		// reset for next cycle after ban expires
		s.failTimes = nil
		s.seenCreds = make(map[string]bool)
		return true
	}
	return false
}

// ClearFailures removes failure records for an IP (called on successful auth).
func (g *Guard) ClearFailures(ip string) {
	s := g.getState(ip)
	s.mu.Lock()
	s.failTimes = nil
	s.seenCreds = make(map[string]bool)
	s.mu.Unlock()
}

func (g *Guard) getState(ip string) *ipState {
	// strip port if present
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}

	g.mu.Lock()
	s, ok := g.states[ip]
	if !ok {
		s = &ipState{seenCreds: make(map[string]bool)}
		g.states[ip] = s
	}
	g.mu.Unlock()
	return s
}

func (g *Guard) gc() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		g.mu.Lock()
		for ip, s := range g.states {
			s.mu.Lock()
			idle := !now.Before(s.bannedUtil) && len(s.failTimes) == 0
			s.mu.Unlock()
			if idle {
				delete(g.states, ip)
			}
		}
		g.mu.Unlock()
	}
}
