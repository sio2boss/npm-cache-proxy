package storage

import (
	"strings"
	"time"

	"github.com/go-redis/redis"
)

// RedisStorage implements Storage interface for Redis database.
type RedisStorage struct {
	Client *redis.Client
}

// NewRedisStorage creates a new Redis storage backend.
func NewRedisStorage(client *redis.Client) *RedisStorage {
	return &RedisStorage{
		Client: client,
	}
}

// Get returns data by key.
func (s *RedisStorage) Get(key string) (string, error) {
	return s.Client.Get(key).Result()
}

// Set stores value identified by key with expiration timeout.
func (s *RedisStorage) Set(key string, value string, expiration time.Duration) error {
	if strings.HasSuffix(key, ":tarball") {
		if err := publishPartialTarball(value); err != nil {
			return err
		}
	}
	return s.Client.Set(key, value, expiration).Err()
}

// RefreshExpiration updates the expiration time for an existing key (sliding expiration).
func (s *RedisStorage) RefreshExpiration(key string, expiration time.Duration) error {
	// Check if key exists first
	exists, err := s.Client.Exists(key).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		// Key doesn't exist, no-op
		return nil
	}
	// Refresh expiration
	return s.Client.Expire(key, expiration).Err()
}

// Delete deletes data by key.
func (s *RedisStorage) Delete(key string) error {
	return s.Client.Del(key).Err()
}

// Keys returns stored keys filtered by prefix.
func (s *RedisStorage) Keys(prefix string) ([]string, error) {
	return s.Client.Keys(prefix + "*").Result()
}

// Health returns an error if database connection cannot be established.
func (s *RedisStorage) Health() error {
	return s.Client.Ping().Err()
}

// Close gracefully closes the Redis connection.
func (s *RedisStorage) Close() error {
	return s.Client.Close()
}
