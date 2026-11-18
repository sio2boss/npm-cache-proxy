package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // Pure-Go SQLite driver, no CGO required
)

// isRetryableError checks if an error is a retryable SQLite lock error
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Check for common SQLite lock/busy error messages
	return strings.Contains(errStr, "database is locked") ||
		strings.Contains(errStr, "Busy") ||
		strings.Contains(errStr, "busy") ||
		strings.Contains(errStr, "SQLITE_BUSY") ||
		strings.Contains(errStr, "SQLITE_LOCKED")
}

// retryOperation executes a database operation with exponential backoff retry logic
func retryOperation(maxRetries int, operation func() error) error {
	var lastErr error
	backoff := 10 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		err := operation()
		if err == nil {
			return nil
		}

		// Only retry if it's a retryable error
		if !isRetryableError(err) {
			return err
		}

		lastErr = err
		if i < maxRetries-1 {
			// Exponential backoff: 10ms, 20ms, 40ms, 80ms, etc.
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	return lastErr
}

// SQLiteStorage implements Storage interface for SQLite database using Turso (in-process SQLite).
type SQLiteStorage struct {
	db                  *sql.DB
	dbPath              string
	packageDir          string
	checkpointInterval  time.Duration
	cleanupInterval     time.Duration
	accessFlushInterval time.Duration
	walSizeAlertMB      int
	stopCheckpoint      chan struct{}
	stopCleanup         chan struct{}
	stopAccessFlush     chan struct{}
	checkpointWg        sync.WaitGroup
	cleanupWg           sync.WaitGroup
	accessFlushWg       sync.WaitGroup
	mu                  sync.RWMutex

	// accessTimes stores key -> accessRecord for sliding expiration.
	// Reads record the configured TTL here; a background flush writes expires_at.
	accessTimes sync.Map
}

// accessRecord is one sliding-expiration update waiting to be flushed.
type accessRecord struct {
	at  int64 // last access, unix seconds
	ttl int64 // TTL in seconds, from the caller
}

// SQLiteConfig provides configuration for SQLite storage.
type SQLiteConfig struct {
	DBPath              string
	PackageDir          string
	CheckpointInterval  time.Duration
	CleanupInterval     time.Duration
	AccessFlushInterval time.Duration // How often to flush in-memory access times to SQLite
	WALSizeAlertMB      int
	MaxOpenConns        int
	MaxIdleConns        int
	ConnMaxLifetime     time.Duration
}

// DefaultSQLiteConfig returns default SQLite configuration.
func DefaultSQLiteConfig() SQLiteConfig {
	return SQLiteConfig{
		DBPath:              "./data/cache.db",
		PackageDir:          "./data/packages",
		CheckpointInterval:  5 * time.Minute,
		CleanupInterval:     1 * time.Hour,
		AccessFlushInterval: 30 * time.Second, // Flush access times every 30 seconds
		WALSizeAlertMB:      50,
		MaxOpenConns:        25,
		MaxIdleConns:        5,
		ConnMaxLifetime:     5 * time.Minute,
	}
}

// NewSQLiteStorage creates a new SQLite storage backend.
func NewSQLiteStorage(config SQLiteConfig) (*SQLiteStorage, error) {
	// Ensure database directory exists
	dbDir := filepath.Dir(config.DBPath)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	// Ensure package directory exists
	if err := os.MkdirAll(config.PackageDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create package directory: %w", err)
	}

	// Open database connection using modernc.org/sqlite driver (pure-Go, no CGO)
	db, err := sql.Open("sqlite", config.DBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)

	// Ensure access flush interval has a default value
	accessFlushInterval := config.AccessFlushInterval
	if accessFlushInterval == 0 {
		accessFlushInterval = 30 * time.Second // Default: flush every 30 seconds
	}

	storage := &SQLiteStorage{
		db:                  db,
		dbPath:              config.DBPath,
		packageDir:          config.PackageDir,
		checkpointInterval:  config.CheckpointInterval,
		cleanupInterval:     config.CleanupInterval,
		accessFlushInterval: accessFlushInterval,
		walSizeAlertMB:      config.WALSizeAlertMB,
		stopCheckpoint:      make(chan struct{}),
		stopCleanup:         make(chan struct{}),
		stopAccessFlush:     make(chan struct{}),
	}

	// Initialize schema
	if err := storage.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Configure SQLite pragmas
	if err := storage.configurePragmas(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to configure pragmas: %w", err)
	}

	// Drop rows and tarball files that expired while the process was down,
	// before background jobs or callers can republish the same paths.
	if err := storage.cleanupExpired(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to clear expired cache entries: %w", err)
	}

	// Start background jobs
	storage.startCheckpointManager()
	storage.startCleanupJob()
	storage.startAccessFlushJob()

	return storage, nil
}

// initSchema creates the required database tables.
func (s *SQLiteStorage) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS registry_cache (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_registry_cache_expires_at ON registry_cache(expires_at);

	CREATE TABLE IF NOT EXISTS packages_metadata (
		package_name TEXT NOT NULL,
		version TEXT NOT NULL,
		metadata TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		PRIMARY KEY (package_name, version)
	);

	CREATE INDEX IF NOT EXISTS idx_packages_metadata_expires_at ON packages_metadata(expires_at);

	CREATE TABLE IF NOT EXISTS tarballs (
		package_name TEXT NOT NULL,
		version TEXT NOT NULL,
		file_path TEXT NOT NULL,
		size INTEGER NOT NULL,
		checksum TEXT,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		PRIMARY KEY (package_name, version)
	);

	CREATE INDEX IF NOT EXISTS idx_tarballs_expires_at ON tarballs(expires_at);
	`

	_, err := s.db.Exec(schema)
	return err
}

// configurePragmas sets SQLite configuration pragmas for optimal performance.
func (s *SQLiteStorage) configurePragmas() error {
	// Set WAL mode for better concurrency (concurrent reads, single writer)
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		// Check if database is already in WAL mode
		var journalMode string
		if checkErr := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); checkErr == nil {
			if journalMode == "wal" || journalMode == "WAL" {
				// Already in WAL mode, that's fine
			} else {
				fmt.Printf("Warning: Could not set journal_mode to WAL: %v\n", err)
			}
		}
	}

	// Set busy_timeout to allow SQLite to wait and retry when database is locked
	// This is critical for handling concurrent access - SQLite will wait up to 5 seconds
	// before returning "database is locked" error
	if _, err := s.db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		// Log warning but don't fail - some SQLite versions may not support this
		fmt.Printf("Warning: Could not set busy_timeout: %v (continuing anyway)\n", err)
	}

	// Try optional pragmas that may not be supported by Turso
	optionalPragmas := []string{
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=-64000", // 64MB
	}

	for _, pragma := range optionalPragmas {
		if _, err := s.db.Exec(pragma); err != nil {
			// Log warning but don't fail - these are optimizations, not requirements
			fmt.Printf("Warning: Could not set %s: %v (continuing anyway)\n", pragma, err)
		}
	}

	return nil
}

// Get returns data by key, filtering expired entries.
func (s *SQLiteStorage) Get(key string) (string, error) {
	// Check if this is a tarball key (ends with :tarball)
	if strings.HasSuffix(key, ":tarball") {
		return s.getTarballMetadata(key)
	}

	// Regular registry cache lookup with retry logic
	now := time.Now().Unix()
	query := `SELECT value FROM registry_cache WHERE key = ? AND expires_at > ?`

	var value string
	var err error

	// Retry up to 3 times for lock errors
	err = retryOperation(3, func() error {
		err := s.db.QueryRow(query, key, now).Scan(&value)
		if err == sql.ErrNoRows {
			// Don't retry "not found" errors
			return err
		}
		return err
	})

	if err == sql.ErrNoRows {
		return "", fmt.Errorf("key not found")
	}
	if err != nil {
		return "", fmt.Errorf("failed to get key: %w", err)
	}

	return value, nil
}

// RefreshExpiration updates the expiration time for an existing key (sliding expiration).
// This is now a fast in-memory operation - access times are flushed to SQLite periodically.
func (s *SQLiteStorage) RefreshExpiration(key string, expiration time.Duration) error {
	ttl := int64(expiration / time.Second)
	if expiration > 0 && ttl == 0 {
		ttl = 1
	}
	s.accessTimes.Store(key, accessRecord{
		at:  time.Now().Unix(),
		ttl: ttl,
	})
	return nil
}

// getTarballMetadata retrieves tarball file path from tarballs table
func (s *SQLiteStorage) getTarballMetadata(key string) (string, error) {
	// Key format: "{prefix}/js-tokens/-/js-tokens-4.0.0.tgz:tarball"
	path, err := tarballPathFromKey(key)
	if err != nil {
		return "", err
	}

	// Path format: "/js-tokens/-/js-tokens-4.0.0.tgz" or "/@types/react/-/react-18.2.0.tgz"
	packageName, version, err := parseTarballPath(path)
	if err != nil {
		return "", fmt.Errorf("failed to parse tarball path: %w", err)
	}

	now := time.Now().Unix()
	query := `SELECT file_path FROM tarballs WHERE package_name = ? AND version = ? AND expires_at > ?`

	var filePath string

	// Retry up to 3 times for lock errors
	err = retryOperation(3, func() error {
		err := s.db.QueryRow(query, packageName, version, now).Scan(&filePath)
		if err == sql.ErrNoRows {
			// Don't retry "not found" errors
			return err
		}
		return err
	})

	if err == sql.ErrNoRows {
		return "", fmt.Errorf("key not found")
	}
	if err != nil {
		return "", fmt.Errorf("failed to get tarball metadata: %w", err)
	}

	return filePath, nil
}

// tarballPathFromKey extracts the URL path from a tarball cache key.
// The storage prefix is not fixed; the path begins at the first '/'.
// Key format: "{prefix}/js-tokens/-/js-tokens-4.0.0.tgz:tarball"
func tarballPathFromKey(key string) (string, error) {
	const suffix = ":tarball"
	if !strings.HasSuffix(key, suffix) {
		return "", fmt.Errorf("invalid tarball key %q", key)
	}
	trimmed := strings.TrimSuffix(key, suffix)
	slash := strings.Index(trimmed, "/")
	if slash < 0 {
		return "", fmt.Errorf("invalid tarball key %q", key)
	}
	return trimmed[slash:], nil
}

// parseTarballPath extracts package name and version from tarball path
// Path format: "/js-tokens/-/js-tokens-4.0.0.tgz" or "/@types/react/-/react-18.2.0.tgz"
func parseTarballPath(path string) (packageName, version string, err error) {
	// Remove leading slash
	path = strings.TrimPrefix(path, "/")

	// Split by "/-/"
	parts := strings.Split(path, "/-/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid tarball path format: %s", path)
	}

	packageName = parts[0]  // "js-tokens" or "@types/react"
	tarballName := parts[1] // "js-tokens-4.0.0.tgz" or "react-18.2.0.tgz"

	// Remove .tgz extension
	tarballName = strings.TrimSuffix(tarballName, ".tgz")

	// Extract version - version is the last part after the last dash
	// For "js-tokens-4.0.0", version is "4.0.0"
	// For "react-18.2.0", version is "18.2.0"
	// But we need to handle scoped packages where the tarball name might not match package name
	// For "@types/react", tarball is "react-18.2.0", so we need to find the version part

	// Find the last dash and everything after it is the version
	lastDash := strings.LastIndex(tarballName, "-")
	if lastDash == -1 {
		return "", "", fmt.Errorf("invalid tarball name format: %s", tarballName)
	}

	version = tarballName[lastDash+1:]

	return packageName, version, nil
}

// Set stores value identified by key with expiration timeout.
// The lock is shared with cleanup so a tarball file cannot be published onto a path that cleanup is unlinking.
func (s *SQLiteStorage) Set(key string, value string, expiration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if this is a tarball key (ends with :tarball)
	if strings.HasSuffix(key, ":tarball") {
		return s.setTarballMetadata(key, value, expiration)
	}

	// Regular registry cache storage with retry logic
	now := time.Now()
	expiresAt := now.Add(expiration).Unix()
	createdAt := now.Unix()

	var err error

	// Retry up to 3 times for lock errors - wrap entire transaction
	err = retryOperation(3, func() error {
		// Use UPDATE then INSERT pattern (more efficient than DELETE then INSERT)
		// Use a transaction to ensure atomicity
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %w", err)
		}
		defer tx.Rollback()

		// Try UPDATE first (if entry exists, update it)
		result, err := tx.Exec(`UPDATE registry_cache SET value = ?, created_at = ?, expires_at = ? WHERE key = ?`,
			value, createdAt, expiresAt, key)
		if err != nil {
			return fmt.Errorf("failed to update key: %w", err)
		}

		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected: %w", err)
		}

		// If no rows were updated, insert new entry
		if rowsAffected == 0 {
			_, err = tx.Exec(`INSERT INTO registry_cache (key, value, created_at, expires_at) VALUES (?, ?, ?, ?)`,
				key, value, createdAt, expiresAt)
			if err != nil {
				return fmt.Errorf("failed to insert key: %w", err)
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}

		return nil
	})

	if err != nil {
		return err
	}

	return nil
}

// setTarballMetadata stores tarball metadata in tarballs table.
// Caller must hold s.mu.
func (s *SQLiteStorage) setTarballMetadata(key string, filePath string, expiration time.Duration) error {
	if err := publishPartialTarball(filePath); err != nil {
		return fmt.Errorf("failed to publish tarball file: %w", err)
	}

	path, err := tarballPathFromKey(key)
	if err != nil {
		return err
	}

	packageName, version, err := parseTarballPath(path)
	if err != nil {
		return fmt.Errorf("failed to parse tarball path: %w", err)
	}

	// Get file size
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat tarball file: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(expiration).Unix()
	createdAt := now.Unix()

	// Retry up to 3 times for lock errors - wrap entire transaction
	err = retryOperation(3, func() error {
		// Use transaction for atomicity
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %w", err)
		}
		defer tx.Rollback()

		// Try UPDATE first (if entry exists, update it)
		result, err := tx.Exec(`UPDATE tarballs SET file_path = ?, size = ?, created_at = ?, expires_at = ? WHERE package_name = ? AND version = ?`,
			filePath, fileInfo.Size(), createdAt, expiresAt, packageName, version)
		if err != nil {
			return fmt.Errorf("failed to update tarball metadata: %w", err)
		}

		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected: %w", err)
		}

		// If no rows were updated, insert new entry
		if rowsAffected == 0 {
			_, err = tx.Exec(`INSERT INTO tarballs (package_name, version, file_path, size, checksum, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				packageName, version, filePath, fileInfo.Size(), "", createdAt, expiresAt)
			if err != nil {
				return fmt.Errorf("failed to insert tarball metadata: %w", err)
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}

		return nil
	})

	if err != nil {
		return err
	}

	return nil
}

// Delete deletes data by key.
func (s *SQLiteStorage) Delete(key string) error {
	// Check if this is a tarball key (ends with :tarball)
	if strings.HasSuffix(key, ":tarball") {
		return s.deleteTarballMetadata(key)
	}

	// Regular registry cache deletion
	query := `DELETE FROM registry_cache WHERE key = ?`
	_, err := s.db.Exec(query, key)
	if err != nil {
		return fmt.Errorf("failed to delete key: %w", err)
	}

	return nil
}

// deleteTarballMetadata deletes tarball metadata from tarballs table
func (s *SQLiteStorage) deleteTarballMetadata(key string) error {
	path, err := tarballPathFromKey(key)
	if err != nil {
		return err
	}

	packageName, version, err := parseTarballPath(path)
	if err != nil {
		return fmt.Errorf("failed to parse tarball path: %w", err)
	}

	query := `DELETE FROM tarballs WHERE package_name = ? AND version = ?`
	_, err = s.db.Exec(query, packageName, version)
	if err != nil {
		return fmt.Errorf("failed to delete tarball metadata: %w", err)
	}

	return nil
}

// Keys returns stored keys filtered by prefix, excluding expired entries.
func (s *SQLiteStorage) Keys(prefix string) ([]string, error) {
	// Don't hold mutex during database query - WAL mode supports concurrent reads
	now := time.Now().Unix()
	query := `SELECT key FROM registry_cache WHERE key LIKE ? AND expires_at > ?`

	rows, err := s.db.Query(query, prefix+"%", now)
	if err != nil {
		return nil, fmt.Errorf("failed to query keys: %w", err)
	}
	defer rows.Close() // CRITICAL: Always close rows to prevent WAL checkpoint issues

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("failed to scan key: %w", err)
		}
		keys = append(keys, key)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	return keys, nil
}

// Health returns an error if database connection cannot be established.
func (s *SQLiteStorage) Health() error {
	// Don't hold mutex during health check - simple read operation
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.db.PingContext(ctx)
}

// Close gracefully closes the SQLite connection and stops background jobs.
func (s *SQLiteStorage) Close() error {
	// Stop background jobs - access flush first to ensure final flush happens
	close(s.stopAccessFlush)
	s.accessFlushWg.Wait()

	close(s.stopCheckpoint)
	close(s.stopCleanup)

	// Wait for jobs to finish
	s.checkpointWg.Wait()
	s.cleanupWg.Wait()

	// Close idle connections to ensure checkpoint can proceed
	// This helps avoid "database is locked" errors during checkpoint
	s.db.SetMaxIdleConns(0)
	s.db.SetMaxOpenConns(1)

	// Final checkpoint before closing
	// Use a separate function that doesn't require the mutex since we're shutting down
	if err := s.performFinalCheckpoint(); err != nil {
		// Log error but don't fail close - database will still be usable
		fmt.Printf("Warning: failed to perform final checkpoint: %v\n", err)
	}

	// Close database connection
	if err := s.db.Close(); err != nil {
		return err
	}

	// Clean up WAL and SHM files after closing connection
	// This ensures VS Code's SQLite Viewer can open the database properly
	dbPath := s.getDBPath()
	shmPath := dbPath + "-shm"
	walPath := dbPath + "-wal"

	// Remove SHM file (shared memory file) - safe to remove when no connections are active
	if err := os.Remove(shmPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("Warning: failed to remove SHM file %s: %v\n", shmPath, err)
	}

	// If WAL file is empty or very small, we can optionally remove it too
	// But typically it's fine to leave an empty WAL file
	if walInfo, err := os.Stat(walPath); err == nil {
		if walInfo.Size() == 0 {
			// Empty WAL file can be removed for cleaner state
			if err := os.Remove(walPath); err != nil && !os.IsNotExist(err) {
				fmt.Printf("Warning: failed to remove empty WAL file %s: %v\n", walPath, err)
			}
		}
	}

	return nil
}

// startCheckpointManager starts the WAL checkpoint manager goroutine.
func (s *SQLiteStorage) startCheckpointManager() {
	s.checkpointWg.Add(1)
	go func() {
		defer s.checkpointWg.Done()

		ticker := time.NewTicker(s.checkpointInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := s.performCheckpoint(); err != nil {
					fmt.Printf("Error performing WAL checkpoint: %v\n", err)
				}
				s.monitorWALSize()
			case <-s.stopCheckpoint:
				return
			}
		}
	}()
}

// performCheckpoint performs a WAL checkpoint operation.
func (s *SQLiteStorage) performCheckpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Use context with timeout to prevent hanging
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try TRUNCATE mode first (most aggressive - truncates WAL after checkpoint)
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err == nil {
		return nil
	}

	// If TRUNCATE fails, try PASSIVE mode (less aggressive, but more likely to succeed)
	// This is useful when there are active read transactions
	_, err = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
	if err == nil {
		return nil
	}

	// If both fail, try RESTART mode (checkpoint and restart WAL, but don't truncate)
	_, err = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(RESTART)")
	if err == nil {
		return nil
	}

	// If all checkpoint modes fail, return the last error
	// This might happen if database is already closed or in an invalid state
	return fmt.Errorf("all checkpoint modes failed, last error: %w", err)
}

// performFinalCheckpoint performs a final checkpoint during shutdown.
// This version doesn't require the mutex since we're shutting down and no other operations should be happening.
func (s *SQLiteStorage) performFinalCheckpoint() error {
	// Use context with timeout to prevent hanging
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Try PASSIVE mode first during shutdown (most likely to succeed with active connections)
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
	if err == nil {
		return nil
	}

	// If PASSIVE fails, try TRUNCATE mode
	_, err = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err == nil {
		return nil
	}

	// If both fail, return error but don't fail the close operation
	return fmt.Errorf("final checkpoint failed: %w", err)
}

// monitorWALSize checks WAL file size and logs warning if it exceeds threshold.
func (s *SQLiteStorage) monitorWALSize() {
	dbPath := s.getDBPath()
	walPath := dbPath + "-wal"

	walInfo, err := os.Stat(walPath)
	if err != nil {
		// WAL file might not exist yet, which is fine
		return
	}

	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		return
	}

	walSizeMB := walInfo.Size() / (1024 * 1024)
	dbSizeMB := dbInfo.Size() / (1024 * 1024)

	if walSizeMB > int64(s.walSizeAlertMB) {
		fmt.Printf("Warning: WAL file size (%d MB) exceeds alert threshold (%d MB). DB size: %d MB\n",
			walSizeMB, s.walSizeAlertMB, dbSizeMB)
	}

	// Alert if WAL is more than 5x the DB size
	if dbSizeMB > 0 && walSizeMB > dbSizeMB*5 {
		fmt.Printf("Warning: WAL file size (%d MB) is more than 5x the database size (%d MB). This may indicate query leaks.\n",
			walSizeMB, dbSizeMB)
	}
}

// getDBPath returns the database file path.
func (s *SQLiteStorage) getDBPath() string {
	return s.dbPath
}

// startCleanupJob starts the expired entry cleanup goroutine.
func (s *SQLiteStorage) startCleanupJob() {
	s.cleanupWg.Add(1)
	go func() {
		defer s.cleanupWg.Done()

		ticker := time.NewTicker(s.cleanupInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := s.cleanupExpired(); err != nil {
					fmt.Printf("Error cleaning up expired entries: %v\n", err)
				}
			case <-s.stopCleanup:
				return
			}
		}
	}()
}

// cleanupExpired removes expired entries from all tables and their tarball files.
// Pending sliding-expiration updates are flushed first so a key accessed since the
// last flush is not deleted. Caller may be startup or the background job.
func (s *SQLiteStorage) cleanupExpired() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.flushAccessTimesLocked(); err != nil {
		return err
	}
	return s.deleteExpiredLocked()
}

// deleteExpiredLocked deletes rows whose expires_at has passed and unlinks tarball
// files for the rows this call removed. Caller must hold s.mu.
func (s *SQLiteStorage) deleteExpiredLocked() error {
	now := time.Now().Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin cleanup transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM registry_cache WHERE expires_at <= ?`, now); err != nil {
		return fmt.Errorf("failed to cleanup registry_cache: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM packages_metadata WHERE expires_at <= ?`, now); err != nil {
		return fmt.Errorf("failed to cleanup packages_metadata: %w", err)
	}

	// RETURNING yields only the rows this statement deleted, and the query is
	// closed before commit so a single SQLite connection cannot deadlock.
	rows, err := tx.Query(`DELETE FROM tarballs WHERE expires_at <= ? RETURNING file_path`, now)
	if err != nil {
		return fmt.Errorf("failed to cleanup tarballs: %w", err)
	}

	var filesToDelete []string
	for rows.Next() {
		var filePath string
		if err := rows.Scan(&filePath); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan expired tarball path: %w", err)
		}
		if filePath != "" {
			filesToDelete = append(filesToDelete, filePath)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("error iterating expired tarballs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("failed to close expired tarball rows: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit cleanup: %w", err)
	}

	for _, filePath := range filesToDelete {
		inUse, err := s.tarballFileReferenced(filePath)
		if err != nil {
			fmt.Printf("Warning: failed to check tarball path %s: %v\n", filePath, err)
			continue
		}
		if inUse {
			continue
		}
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("Warning: failed to remove expired tarball %s: %v\n", filePath, err)
		}
	}

	return nil
}

// tarballFileReferenced reports whether any tarball row still points at filePath.
func (s *SQLiteStorage) tarballFileReferenced(filePath string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tarballs WHERE file_path = ?`, filePath).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// startAccessFlushJob starts the background job that flushes in-memory access times to SQLite.
// This enables sliding expiration without per-request database writes.
func (s *SQLiteStorage) startAccessFlushJob() {
	s.accessFlushWg.Add(1)
	go func() {
		defer s.accessFlushWg.Done()

		ticker := time.NewTicker(s.accessFlushInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := s.flushAccessTimes(); err != nil {
					fmt.Printf("Error flushing access times: %v\n", err)
				}
			case <-s.stopAccessFlush:
				// Final flush before stopping
				if err := s.flushAccessTimes(); err != nil {
					fmt.Printf("Error during final access time flush: %v\n", err)
				}
				return
			}
		}
	}()
}

// flushAccessTimes writes all in-memory access times to SQLite in a single batch.
func (s *SQLiteStorage) flushAccessTimes() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushAccessTimesLocked()
}

// flushAccessTimesLocked writes pending sliding expirations. Caller must hold s.mu.
// Records are removed only after commit, and only when the map still holds the
// value that was written, so a newer access or a failed commit is retried.
func (s *SQLiteStorage) flushAccessTimesLocked() error {
	type pendingAccess struct {
		key    string
		record accessRecord
	}
	var entries []pendingAccess

	s.accessTimes.Range(func(key, value interface{}) bool {
		rec, ok := value.(accessRecord)
		if !ok {
			s.accessTimes.Delete(key)
			return true
		}
		entries = append(entries, pendingAccess{key: key.(string), record: rec})
		return true
	})

	if len(entries) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	registryStmt, err := tx.Prepare(`UPDATE registry_cache SET expires_at = ? WHERE key = ?`)
	if err != nil {
		return fmt.Errorf("failed to prepare registry update: %w", err)
	}
	defer registryStmt.Close()

	tarballStmt, err := tx.Prepare(`UPDATE tarballs SET expires_at = ? WHERE package_name = ? AND version = ?`)
	if err != nil {
		return fmt.Errorf("failed to prepare tarball update: %w", err)
	}
	defer tarballStmt.Close()

	applied := make([]pendingAccess, 0, len(entries))
	for _, entry := range entries {
		newExpiresAt := entry.record.at + entry.record.ttl

		if strings.HasSuffix(entry.key, ":tarball") {
			path, err := tarballPathFromKey(entry.key)
			if err != nil {
				fmt.Printf("Warning: skipping tarball expiration update for %s: %v\n", entry.key, err)
				s.accessTimes.CompareAndDelete(entry.key, entry.record)
				continue
			}
			packageName, version, err := parseTarballPath(path)
			if err != nil {
				fmt.Printf("Warning: skipping tarball expiration update for %s: %v\n", entry.key, err)
				s.accessTimes.CompareAndDelete(entry.key, entry.record)
				continue
			}
			if _, err = tarballStmt.Exec(newExpiresAt, packageName, version); err != nil {
				fmt.Printf("Warning: failed to update tarball expiration for %s: %v\n", entry.key, err)
				continue
			}
		} else if _, err = registryStmt.Exec(newExpiresAt, entry.key); err != nil {
			fmt.Printf("Warning: failed to update registry expiration for %s: %v\n", entry.key, err)
			continue
		}

		applied = append(applied, entry)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit access time flush: %w", err)
	}

	for _, entry := range applied {
		s.accessTimes.CompareAndDelete(entry.key, entry.record)
	}

	return nil
}
