package storage

import (
	"time"
)

// Storage provides interface for data storage backends.
// This interface abstracts cache operations from the underlying storage mechanism.
type Storage interface {
	// Get returns data by key.
	Get(key string) (string, error)

	// Set stores value identified by key with expiration timeout.
	Set(key string, value string, expiration time.Duration) error

	// RefreshExpiration updates the expiration time for an existing key (sliding expiration).
	// If the key doesn't exist, this is a no-op (no error returned).
	RefreshExpiration(key string, expiration time.Duration) error

	// Delete deletes data by key.
	Delete(key string) error

	// Keys returns stored keys filtered by prefix.
	Keys(prefix string) ([]string, error)

	// Health returns an error if database connection cannot be established.
	Health() error

	// Close gracefully closes the storage connection.
	// This is important for SQLite to ensure WAL checkpoints and cleanup.
	Close() error
}
