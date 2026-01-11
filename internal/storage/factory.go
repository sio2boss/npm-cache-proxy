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
	SQLiteDBPath             string
	SQLitePackageDir         string
	SQLiteCheckpointInterval time.Duration
	SQLiteCleanupInterval    time.Duration
	SQLiteWALSizeAlertMB     int
	SQLiteMaxOpenConns       int
	SQLiteMaxIdleConns       int
	SQLiteConnMaxLifetime    time.Duration
}

// DefaultConfig returns default configuration.
func DefaultConfig() Config {
	return Config{
		Backend:                 BackendSQLite,
		RedisAddress:           "localhost:6379",
		RedisPassword:           "",
		RedisDatabase:          0,
		SQLiteDBPath:           "./data/cache.db",
		SQLitePackageDir:       "./data/packages",
		SQLiteCheckpointInterval: 5 * time.Minute,
		SQLiteCleanupInterval:   1 * time.Hour,
		SQLiteWALSizeAlertMB:    50,
		SQLiteMaxOpenConns:      25,
		SQLiteMaxIdleConns:      5,
		SQLiteConnMaxLifetime:   5 * time.Minute,
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
			DBPath:             config.SQLiteDBPath,
			PackageDir:         config.SQLitePackageDir,
			CheckpointInterval: config.SQLiteCheckpointInterval,
			CleanupInterval:    config.SQLiteCleanupInterval,
			WALSizeAlertMB:     config.SQLiteWALSizeAlertMB,
			MaxOpenConns:       config.SQLiteMaxOpenConns,
			MaxIdleConns:       config.SQLiteMaxIdleConns,
			ConnMaxLifetime:    config.SQLiteConnMaxLifetime,
		}
		return NewSQLiteStorage(sqliteConfig)

	default:
		return nil, fmt.Errorf("unsupported storage backend: %s", config.Backend)
	}
}
