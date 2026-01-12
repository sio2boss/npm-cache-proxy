package storage

import (
	"fmt"
	"time"

	"github.com/go-redis/redis"
)

// BackendType represents the storage backend type.
type BackendType string

const (
	BackendRedis  BackendType = "redis"
	BackendSQLite BackendType = "sqlite"
)

// Config holds configuration for storage backend creation.
type Config struct {
	Backend BackendType

	// Redis configuration
	RedisAddress  string
	RedisPassword string
	RedisDatabase int

	// SQLite configuration
	SQLiteDBPath              string
	SQLitePackageDir          string
	SQLiteCheckpointInterval  time.Duration
	SQLiteCleanupInterval     time.Duration
	SQLiteAccessFlushInterval time.Duration
	SQLiteWALSizeAlertMB      int
	SQLiteMaxOpenConns        int
	SQLiteMaxIdleConns        int
	SQLiteConnMaxLifetime     time.Duration

	// L1 Cache configuration
	EnableL1Cache  bool
	L1CacheMaxAge  time.Duration
}

// DefaultConfig returns default configuration.
func DefaultConfig() Config {
	return Config{
		Backend:                   BackendSQLite,
		RedisAddress:              "localhost:6379",
		RedisPassword:             "",
		RedisDatabase:             0,
		SQLiteDBPath:              "./data/cache.db",
		SQLitePackageDir:          "./data/packages",
		SQLiteCheckpointInterval:  5 * time.Minute,
		SQLiteCleanupInterval:     1 * time.Hour,
		SQLiteAccessFlushInterval: 30 * time.Second,
		SQLiteWALSizeAlertMB:      50,
		SQLiteMaxOpenConns:        1, // Single connection for SQLite to avoid lock contention
		SQLiteMaxIdleConns:        1,
		SQLiteConnMaxLifetime:     0, // No expiration
		EnableL1Cache:             true,
		L1CacheMaxAge:             5 * time.Minute,
	}
}

// NewStorage creates a new storage backend based on configuration.
func NewStorage(config Config) (Storage, error) {
	switch config.Backend {
	case BackendRedis:
		return NewRedisStorage(redis.NewClient(&redis.Options{
			Addr:     config.RedisAddress,
			Password: config.RedisPassword,
			DB:       config.RedisDatabase,
		})), nil

	case BackendSQLite:
		sqliteConfig := SQLiteConfig{
			DBPath:              config.SQLiteDBPath,
			PackageDir:          config.SQLitePackageDir,
			CheckpointInterval:  config.SQLiteCheckpointInterval,
			CleanupInterval:     config.SQLiteCleanupInterval,
			AccessFlushInterval: config.SQLiteAccessFlushInterval,
			WALSizeAlertMB:      config.SQLiteWALSizeAlertMB,
			MaxOpenConns:        config.SQLiteMaxOpenConns,
			MaxIdleConns:        config.SQLiteMaxIdleConns,
			ConnMaxLifetime:     config.SQLiteConnMaxLifetime,
		}
		sqliteStorage, err := NewSQLiteStorage(sqliteConfig)
		if err != nil {
			return nil, err
		}

		// Wrap with L1 cache if enabled
		if config.EnableL1Cache {
			return NewCachedStorage(CachedStorageConfig{
				Backend: sqliteStorage,
				MaxAge:  config.L1CacheMaxAge,
			}), nil
		}

		return sqliteStorage, nil

	default:
		return nil, fmt.Errorf("unsupported storage backend: %s", config.Backend)
	}
}
