package server

import (
	"sync"
	"time"
)

// ─── Data cache (Supabase query results) ────────────────────────────────────

type cacheItem struct {
	data      []byte
	expiresAt time.Time
}

var (
	cacheMu sync.RWMutex
	cache   = map[string]*cacheItem{}
)

func cacheGet(key string) []byte {
	cacheMu.RLock()
	item, ok := cache[key]
	cacheMu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(item.expiresAt) {
		cacheMu.Lock()
		// Re-check after acquiring write lock — another goroutine may have
		// inserted a fresh entry between our RUnlock and Lock.
		if item2, ok2 := cache[key]; ok2 && time.Now().After(item2.expiresAt) {
			delete(cache, key)
		}
		cacheMu.Unlock()
		return nil
	}
	return item.data
}

func cacheSet(key string, data []byte, ttl time.Duration) {
	cacheMu.Lock()
	cache[key] = &cacheItem{data: data, expiresAt: time.Now().Add(ttl)}
	cacheMu.Unlock()
}

func cacheDelete(key string) {
	cacheMu.Lock()
	delete(cache, key)
	cacheMu.Unlock()
}

func cacheClear() {
	cacheMu.Lock()
	cache = map[string]*cacheItem{}
	cacheMu.Unlock()
}

// InvalidateVideosCacheFn is set by the router package to avoid import cycles.
// It forces scanVideosCached() to re-scan on the next request.
var InvalidateVideosCacheFn func()

// InvalidateAllCaches clears all caches. Call after any data mutation.
func InvalidateAllCaches() {
	cacheClear()
	InvalidatePageCache()
	if InvalidateVideosCacheFn != nil {
		InvalidateVideosCacheFn()
	}
}

// ─── Page cache tracking ────────────────────────────────────────────────────

var (
	pageMu     sync.RWMutex
	pageCached = map[string]bool{}
)

const pageCacheDuration = 60 * time.Second

func InvalidatePageCache() {
	pageMu.Lock()
	pageCached = map[string]bool{}
	pageMu.Unlock()
}

func IsPageCached(url string) bool {
	pageMu.RLock()
	_, ok := pageCached[url]
	pageMu.RUnlock()
	return ok
}

func MarkPageCached(url string) {
	pageMu.Lock()
	pageCached[url] = true
	pageMu.Unlock()
}
