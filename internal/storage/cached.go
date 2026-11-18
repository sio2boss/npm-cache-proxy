package storage

import (
	"sync"
	"time"
)

// CachedStorage wraps a Storage implementation with an in-memory L1 cache.
// This provides sub-microsecond access for frequently accessed data while
// falling back to the underlying storage for cache misses.
type CachedStorage struct {
	backend     Storage
	cache       sync.Map // key -> cachedEntry
	maxAge      time.Duration
	stopJanitor chan struct{}
	janitorWg   sync.WaitGroup
	closeOnce   sync.Once
}

// cachedEntry stores a cached value and the times that bound its L1 lifetime.
// expiresAt is the backend TTL captured at Set. A zero expiresAt means the
// entry was filled from Get and is bounded only by maxAge.
type cachedEntry struct {
	value     string
	timestamp time.Time
	expiresAt time.Time
}

func (e cachedEntry) fresh(now time.Time, maxAge time.Duration) bool {
	if now.Sub(e.timestamp) >= maxAge {
		return false
	}
	if !e.expiresAt.IsZero() && !now.Before(e.expiresAt) {
		return false
	}
	return true
}

// CachedStorageConfig provides configuration for CachedStorage.
type CachedStorageConfig struct {
	Backend Storage
	MaxAge  time.Duration // How long entries stay in L1 cache (default: 5 minutes)
}

// NewCachedStorage creates a new cached storage wrapper.
func NewCachedStorage(config CachedStorageConfig) *CachedStorage {
	maxAge := config.MaxAge
	if maxAge == 0 {
		maxAge = 5 * time.Minute
	}

	c := &CachedStorage{
		backend:     config.Backend,
		maxAge:      maxAge,
		stopJanitor: make(chan struct{}),
	}
	c.startJanitor()
	return c
}

func (c *CachedStorage) startJanitor() {
	c.janitorWg.Add(1)
	go func() {
		defer c.janitorWg.Done()

		ticker := time.NewTicker(c.maxAge)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.evictStale()
			case <-c.stopJanitor:
				return
			}
		}
	}()
}

// evictStale drops L1 entries that are past maxAge or their captured TTL.
func (c *CachedStorage) evictStale() {
	now := time.Now()
	c.cache.Range(func(key, value interface{}) bool {
		cached := value.(cachedEntry)
		if !cached.fresh(now, c.maxAge) {
			c.cache.CompareAndDelete(key, cached)
		}
		return true
	})
}

// Get retrieves a value, checking L1 cache first before falling back to backend.
func (c *CachedStorage) Get(key string) (string, error) {
	if entry, ok := c.cache.Load(key); ok {
		cached := entry.(cachedEntry)
		if cached.fresh(time.Now(), c.maxAge) {
			return cached.value, nil
		}
		c.cache.CompareAndDelete(key, cached)
	}

	value, err := c.backend.Get(key)
	if err != nil {
		return "", err
	}

	// Filled from the backend, so the remaining TTL is unknown. maxAge is the
	// revalidation window; RefreshExpiration does not extend it.
	c.cache.Store(key, cachedEntry{
		value:     value,
		timestamp: time.Now(),
	})

	return value, nil
}

// Set stores a value in both L1 cache and backend.
func (c *CachedStorage) Set(key string, value string, expiration time.Duration) error {
	if err := c.backend.Set(key, value, expiration); err != nil {
		return err
	}

	now := time.Now()
	entry := cachedEntry{
		value:     value,
		timestamp: now,
	}
	if expiration > 0 {
		entry.expiresAt = now.Add(expiration)
	}
	c.cache.Store(key, entry)

	return nil
}

// RefreshExpiration records a backend sliding expiration and leaves the L1
// entry alone. Extending L1 here would keep serving a value after cleanup
// deleted the backend row.
func (c *CachedStorage) RefreshExpiration(key string, expiration time.Duration) error {
	return c.backend.RefreshExpiration(key, expiration)
}

// Delete removes a value from both L1 cache and backend.
func (c *CachedStorage) Delete(key string) error {
	c.cache.Delete(key)
	return c.backend.Delete(key)
}

// Keys delegates to the backend.
func (c *CachedStorage) Keys(prefix string) ([]string, error) {
	return c.backend.Keys(prefix)
}

// Health delegates to the backend.
func (c *CachedStorage) Health() error {
	return c.backend.Health()
}

// Close stops the L1 janitor and closes the underlying backend.
func (c *CachedStorage) Close() error {
	c.closeOnce.Do(func() {
		close(c.stopJanitor)
	})
	c.janitorWg.Wait()
	return c.backend.Close()
}

// ClearCache clears all entries from the L1 cache.
// Useful for testing or manual cache invalidation.
func (c *CachedStorage) ClearCache() {
	c.cache.Range(func(key, value interface{}) bool {
		c.cache.Delete(key)
		return true
	})
}

// CacheStats returns statistics about the L1 cache.
func (c *CachedStorage) CacheStats() (entries int, staleEntries int) {
	now := time.Now()
	c.cache.Range(func(key, value interface{}) bool {
		entries++
		cached := value.(cachedEntry)
		if !cached.fresh(now, c.maxAge) {
			staleEntries++
		}
		return true
	})
	return
}
