package proxy

import (
	"time"

	"github.com/go-redis/redis"
)

// DatabaseRedis implements Database interface for Redis database.
type DatabaseRedis struct {
	Client *redis.Client
}

// Get returns data by key.
func (db DatabaseRedis) Get(key string) (string, error) {
	return db.Client.Get(key).Result()
}

// Set stores value identified by key with expiration timeout.
func (db DatabaseRedis) Set(key string, value string, expiration time.Duration) error {
	return db.Client.Set(key, value, expiration).Err()
}

// RefreshExpiration updates the expiration time for an existing key (sliding expiration).
func (db DatabaseRedis) RefreshExpiration(key string, expiration time.Duration) error {
	// Check if key exists first
	exists, err := db.Client.Exists(key).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		// Key doesn't exist, no-op
		return nil
	}
	// Refresh expiration
	return db.Client.Expire(key, expiration).Err()
}

// Delete deletes data by key.
func (db DatabaseRedis) Delete(key string) error {
	return db.Client.Del(key).Err()
}

// Keys returns stored keys filtered by prefix.
func (db DatabaseRedis) Keys(prefix string) ([]string, error) {
	return db.Client.Keys(prefix + "*").Result()
}

// Health returns an error if database connection cannot be estabilished.
func (db DatabaseRedis) Health() error {
	return db.Client.Ping().Err()
}
