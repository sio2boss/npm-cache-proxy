package storage

import (
	"sync"
	"time"
)

// CachedStorage wraps a Storage implementation with an in-memory L1 cache.
// This provides sub-microsecond access for frequently accessed data while
// falling back to the underlying storage for cache misses.
type CachedStorage struct {
	backend Storage
	cache   sync.Map // key -> cachedEntry
	maxAge  time.Duration
}

// cachedEntry stores a cached value with its timestamp
type cachedEntry struct {
	value     string
	timestamp time.Time
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

	return &CachedStorage{
		backend: config.Backend,
		maxAge:  maxAge,
	}
}

// Get retrieves a value, checking L1 cache first before falling back to backend.
func (c *CachedStorage) Get(key string) (string, error) {
	// Check L1 cache first
	if entry, ok := c.cache.Load(key); ok {
		cached := entry.(cachedEntry)
		// Check if entry is still fresh
		if time.Since(cached.timestamp) < c.maxAge {
			return cached.value, nil
		}
		// Entry is stale, remove it
		c.cache.Delete(key)
	}

	// L1 miss - fetch from backend (L2)
	value, err := c.backend.Get(key)
	if err != nil {
		return "", err
	}

	// Store in L1 cache
	c.cache.Store(key, cachedEntry{
		value:     value,
		timestamp: time.Now(),
	})

	return value, nil
}

// Set stores a value in both L1 cache and backend.
func (c *CachedStorage) Set(key string, value string, expiration time.Duration) error {
	// Write to backend first
	if err := c.backend.Set(key, value, expiration); err != nil {
		return err
	}

	// Update L1 cache
	c.cache.Store(key, cachedEntry{
		value:     value,
		timestamp: time.Now(),
	})

	return nil
}

// RefreshExpiration delegates to the backend (which now uses in-memory tracking).
func (c *CachedStorage) RefreshExpiration(key string, expiration time.Duration) error {
	// Update L1 cache timestamp to keep it fresh
	if entry, ok := c.cache.Load(key); ok {
		cached := entry.(cachedEntry)
		c.cache.Store(key, cachedEntry{
			value:     cached.value,
			timestamp: time.Now(),
		})
	}

	// Delegate to backend
	return c.backend.RefreshExpiration(key, expiration)
}

// Delete removes a value from both L1 cache and backend.
func (c *CachedStorage) Delete(key string) error {
	// Remove from L1 cache
	c.cache.Delete(key)

	// Delete from backend
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

// Close closes the underlying backend.
func (c *CachedStorage) Close() error {
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
		if now.Sub(cached.timestamp) >= c.maxAge {
			staleEntries++
		}
		return true
	})
	return
}
