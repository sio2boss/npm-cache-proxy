package storage

import (
	"fmt"
	"testing"
	"time"
)

type stubStorage struct {
	values    map[string]string
	gets      int
	refreshed time.Duration
}

func (s *stubStorage) Get(key string) (string, error) {
	s.gets++
	value, ok := s.values[key]
	if !ok {
		return "", fmt.Errorf("key not found")
	}
	return value, nil
}

func (s *stubStorage) Set(key string, value string, expiration time.Duration) error {
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
	return nil
}

func (s *stubStorage) RefreshExpiration(key string, expiration time.Duration) error {
	s.refreshed = expiration
	return nil
}

func (s *stubStorage) Delete(key string) error {
	delete(s.values, key)
	return nil
}

func (s *stubStorage) Keys(prefix string) ([]string, error) { return nil, nil }
func (s *stubStorage) Health() error                        { return nil }
func (s *stubStorage) Close() error                         { return nil }

func TestL1ExpiresBeforeMaxAge(t *testing.T) {
	backend := &stubStorage{values: map[string]string{}}
	cache := NewCachedStorage(CachedStorageConfig{Backend: backend, MaxAge: time.Hour})
	defer cache.Close()

	if err := cache.Set("pkg", "v1", 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	backend.values["pkg"] = "v2"

	time.Sleep(40 * time.Millisecond)
	got, err := cache.Get("pkg")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v2" {
		t.Fatalf("Get = %q, want refreshed backend value", got)
	}
	if backend.gets != 1 {
		t.Fatalf("backend gets = %d, want 1", backend.gets)
	}
}

func TestL1RefreshDoesNotExtendLifetime(t *testing.T) {
	backend := &stubStorage{values: map[string]string{}}
	cache := NewCachedStorage(CachedStorageConfig{Backend: backend, MaxAge: time.Hour})
	defer cache.Close()

	if err := cache.Set("pkg", "v1", time.Hour); err != nil {
		t.Fatal(err)
	}
	loaded, _ := cache.cache.Load("pkg")
	before := loaded.(cachedEntry)

	if err := cache.RefreshExpiration("pkg", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if backend.refreshed != 2*time.Hour {
		t.Fatalf("backend refresh = %s", backend.refreshed)
	}
	loaded, _ = cache.cache.Load("pkg")
	after := loaded.(cachedEntry)
	if !after.timestamp.Equal(before.timestamp) || !after.expiresAt.Equal(before.expiresAt) {
		t.Fatalf("L1 entry changed on refresh: before=%+v after=%+v", before, after)
	}
}

func TestL1JanitorDropsStaleEntries(t *testing.T) {
	cache := NewCachedStorage(CachedStorageConfig{Backend: &stubStorage{}, MaxAge: time.Hour})
	defer cache.Close()

	cache.cache.Store("old", cachedEntry{
		value:     "stale",
		timestamp: time.Now().Add(-2 * time.Hour),
	})
	cache.cache.Store("ttl", cachedEntry{
		value:     "expired",
		timestamp: time.Now(),
		expiresAt: time.Now().Add(-time.Second),
	})
	cache.cache.Store("fresh", cachedEntry{
		value:     "ok",
		timestamp: time.Now(),
		expiresAt: time.Now().Add(time.Hour),
	})

	cache.evictStale()

	if _, ok := cache.cache.Load("old"); ok {
		t.Fatal("aged entry was not evicted")
	}
	if _, ok := cache.cache.Load("ttl"); ok {
		t.Fatal("TTL-expired entry was not evicted")
	}
	if _, ok := cache.cache.Load("fresh"); !ok {
		t.Fatal("fresh entry was evicted")
	}
}
