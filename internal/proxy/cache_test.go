package proxy

import (
	"testing"
	"time"

	"github.com/LocalImages/LocalImage/internal/session"
)

func TestEvictStaleKeepsOnlyRecentLookups(t *testing.T) {
	now := time.Now()
	p := &Proxy{cache: map[string]cacheEntry{
		"fresh":   {rec: &session.Record{ID: "a"}, fetched: now.Add(-1 * time.Second)},
		"expired": {rec: &session.Record{ID: "b"}, fetched: now.Add(-cacheTTL - time.Second)},
		"old":     {rec: &session.Record{ID: "c"}, fetched: now.Add(-time.Hour)},
	}}
	p.evictStale(now)
	if len(p.cache) != 1 || p.cache["fresh"].rec == nil {
		t.Fatalf("cache after eviction = %v, want only the fresh entry", p.cache)
	}
}

func TestCacheDoesNotGrowWithoutBound(t *testing.T) {
	p := &Proxy{cache: map[string]cacheEntry{}}
	base := time.Now()
	for i := 0; i < 10000; i++ {
		// A new session is looked up every 100ms, as a busy daemon might see.
		now := base.Add(time.Duration(i) * 100 * time.Millisecond)
		p.evictStale(now)
		p.cache[string(rune(i))+"-token"] = cacheEntry{rec: &session.Record{}, fetched: now}
	}
	if len(p.cache) > int(cacheTTL/(100*time.Millisecond))+2 {
		t.Errorf("cache holds %d entries after 10000 lookups; it should only hold the last %s worth", len(p.cache), cacheTTL)
	}
}
