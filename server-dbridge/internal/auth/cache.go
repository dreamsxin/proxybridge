package auth

import (
	"sync"
	"time"

	"dbridge/internal/center"
)

type cacheEntry struct {
	result    *center.AuthResponse
	expiresAt time.Time
}

// successCache stores successful auth results keyed by "user:pass".
type successCache struct {
	mu    sync.RWMutex
	items map[string]cacheEntry
}

func newSuccessCache() *successCache {
	c := &successCache{items: make(map[string]cacheEntry)}
	go c.gc()
	return c
}

func (c *successCache) get(key string) (*center.AuthResponse, bool) {
	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.result, true
}

func (c *successCache) set(key string, result *center.AuthResponse) {
	c.mu.Lock()
	c.items[key] = cacheEntry{result: result, expiresAt: time.Now().Add(2 * time.Hour)}
	c.mu.Unlock()
}

func (c *successCache) delete(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

func (c *successCache) gc() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		c.mu.Lock()
		for k, e := range c.items {
			if now.After(e.expiresAt) {
				delete(c.items, k)
			}
		}
		c.mu.Unlock()
	}
}

// failCache stores failed auth results keyed by "user:pass" with 1-minute TTL.
type failCache struct {
	mu    sync.RWMutex
	items map[string]time.Time // key → expiry
}

func newFailCache() *failCache {
	c := &failCache{items: make(map[string]time.Time)}
	go c.gc()
	return c
}

func (c *failCache) has(key string) bool {
	c.mu.RLock()
	exp, ok := c.items[key]
	c.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

func (c *failCache) set(key string) {
	c.mu.Lock()
	c.items[key] = time.Now().Add(time.Minute)
	c.mu.Unlock()
}

func (c *failCache) gc() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		c.mu.Lock()
		for k, exp := range c.items {
			if now.After(exp) {
				delete(c.items, k)
			}
		}
		c.mu.Unlock()
	}
}
