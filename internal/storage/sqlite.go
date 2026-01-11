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

	_ "github.com/tursodatabase/turso-go"
)

// SQLiteStorage implements Storage interface for SQLite database using Turso (in-process SQLite).
type SQLiteStorage struct {
	db                    *sql.DB
	dbPath                string
	packageDir            string
	checkpointInterval    time.Duration
	cleanupInterval       time.Duration
	walSizeAlertMB        int
	stopCheckpoint        chan struct{}
	stopCleanup           chan struct{}
	checkpointWg          sync.WaitGroup
	cleanupWg             sync.WaitGroup
	mu                    sync.RWMutex
}

// SQLiteConfig provides configuration for SQLite storage.
type SQLiteConfig struct {
	DBPath             string
	PackageDir         string
	CheckpointInterval time.Duration
	CleanupInterval    time.Duration
	WALSizeAlertMB     int
	MaxOpenConns       int
	MaxIdleConns       int
	ConnMaxLifetime    time.Duration
}

// DefaultSQLiteConfig returns default SQLite configuration.
func DefaultSQLiteConfig() SQLiteConfig {
	return SQLiteConfig{
		DBPath:             "./data/cache.db",
		PackageDir:         "./data/packages",
		CheckpointInterval: 5 * time.Minute,
		CleanupInterval:    1 * time.Hour,
		WALSizeAlertMB:     50,
		MaxOpenConns:       25,
		MaxIdleConns:       5,
		ConnMaxLifetime:    5 * time.Minute,
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

	// Open database connection using Turso driver (in-process SQLite)
	// Turso supports WAL mode and better concurrency than standard SQLite
	db, err := sql.Open("turso", config.DBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)

	storage := &SQLiteStorage{
		db:                 db,
		dbPath:             config.DBPath,
		packageDir:         config.PackageDir,
		checkpointInterval: config.CheckpointInterval,
		cleanupInterval:    config.CleanupInterval,
		walSizeAlertMB:     config.WALSizeAlertMB,
		stopCheckpoint:    make(chan struct{}),
		stopCleanup:        make(chan struct{}),
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

	// Start background jobs
	storage.startCheckpointManager()
	storage.startCleanupJob()

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

// configurePragmas sets SQLite configuration pragmas.
// Note: Turso may not support all standard SQLite pragmas, so we handle errors gracefully
func (s *SQLiteStorage) configurePragmas() error {
	// Try to set WAL mode first (Turso should support this)
	// If it fails, log a warning but continue - Turso may handle WAL mode automatically
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		// Check if database is already in WAL mode or if Turso handles it differently
		var journalMode string
		if checkErr := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); checkErr == nil {
			if journalMode == "wal" || journalMode == "WAL" {
				// Already in WAL mode, that's fine
				return nil
			}
		}
		// If setting WAL fails and we're not already in WAL, log warning but don't fail
		// Turso may handle WAL mode automatically or have different requirements
		fmt.Printf("Warning: Could not set journal_mode to WAL: %v (continuing anyway - Turso may handle this automatically)\n", err)
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

	// Regular registry cache lookup
	now := time.Now().Unix()
	query := `SELECT value FROM registry_cache WHERE key = ? AND expires_at > ?`

	var value string
	err := s.db.QueryRow(query, key, now).Scan(&value)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("key not found")
	}
	if err != nil {
		return "", fmt.Errorf("failed to get key: %w", err)
	}

	return value, nil
}

// RefreshExpiration updates the expiration time for an existing key (sliding expiration).
func (s *SQLiteStorage) RefreshExpiration(key string, expiration time.Duration) error {
	// Check if this is a tarball key (ends with :tarball)
	if strings.HasSuffix(key, ":tarball") {
		return s.refreshTarballExpiration(key, expiration)
	}

	// Regular registry cache expiration refresh
	now := time.Now()
	expiresAt := now.Add(expiration).Unix()

	// Update expiration time if key exists
	result, err := s.db.Exec(`UPDATE registry_cache SET expires_at = ? WHERE key = ? AND expires_at > ?`,
		expiresAt, key, now.Unix())
	if err != nil {
		return fmt.Errorf("failed to refresh expiration: %w", err)
	}

	// Check if any rows were affected (key exists and wasn't expired)
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	// If no rows affected, key doesn't exist or is expired - that's okay, no-op
	if rowsAffected == 0 {
		return nil
	}

	return nil
}

// refreshTarballExpiration updates expiration for tarball metadata
func (s *SQLiteStorage) refreshTarballExpiration(key string, expiration time.Duration) error {
	// Extract path from key (remove prefix and :tarball suffix)
	path := strings.TrimPrefix(key, "ncp-")
	path = strings.TrimSuffix(path, ":tarball")
	
	// Parse package name and version from path
	packageName, version, err := parseTarballPath(path)
	if err != nil {
		return fmt.Errorf("failed to parse tarball path: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(expiration).Unix()

	// Update expiration time if entry exists and isn't expired
	result, err := s.db.Exec(`UPDATE tarballs SET expires_at = ? WHERE package_name = ? AND version = ? AND expires_at > ?`,
		expiresAt, packageName, version, now.Unix())
	if err != nil {
		return fmt.Errorf("failed to refresh tarball expiration: %w", err)
	}

	// Check if any rows were affected
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	// If no rows affected, entry doesn't exist or is expired - that's okay, no-op
	if rowsAffected == 0 {
		return nil
	}

	return nil
}

// getTarballMetadata retrieves tarball file path from tarballs table
func (s *SQLiteStorage) getTarballMetadata(key string) (string, error) {
	// Extract path from key (remove prefix and :tarball suffix)
	// Key format: "ncp-/js-tokens/-/js-tokens-4.0.0.tgz:tarball"
	// Path format: "/js-tokens/-/js-tokens-4.0.0.tgz"
	path := strings.TrimPrefix(key, "ncp-")
	path = strings.TrimSuffix(path, ":tarball")
	
	// Parse package name and version from path
	// Path format: "/js-tokens/-/js-tokens-4.0.0.tgz" or "/@types/react/-/react-18.2.0.tgz"
	packageName, version, err := parseTarballPath(path)
	if err != nil {
		return "", fmt.Errorf("failed to parse tarball path: %w", err)
	}

	now := time.Now().Unix()
	query := `SELECT file_path FROM tarballs WHERE package_name = ? AND version = ? AND expires_at > ?`

	var filePath string
	err = s.db.QueryRow(query, packageName, version, now).Scan(&filePath)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("key not found")
	}
	if err != nil {
		return "", fmt.Errorf("failed to get tarball metadata: %w", err)
	}

	return filePath, nil
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
	
	packageName = parts[0] // "js-tokens" or "@types/react"
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
func (s *SQLiteStorage) Set(key string, value string, expiration time.Duration) error {
	// Check if this is a tarball key (ends with :tarball)
	if strings.HasSuffix(key, ":tarball") {
		return s.setTarballMetadata(key, value, expiration)
	}

	// Regular registry cache storage
	now := time.Now()
	expiresAt := now.Add(expiration).Unix()
	createdAt := now.Unix()

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
}

// setTarballMetadata stores tarball metadata in tarballs table
func (s *SQLiteStorage) setTarballMetadata(key string, filePath string, expiration time.Duration) error {
	// Extract path from key (remove prefix and :tarball suffix)
	path := strings.TrimPrefix(key, "ncp-")
	path = strings.TrimSuffix(path, ":tarball")
	
	// Parse package name and version from path
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
	// Extract path from key (remove prefix and :tarball suffix)
	path := strings.TrimPrefix(key, "ncp-")
	path = strings.TrimSuffix(path, ":tarball")
	
	// Parse package name and version from path
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
	// Stop background jobs
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

// cleanupExpired removes expired entries from all tables and orphaned tarball files.
func (s *SQLiteStorage) cleanupExpired() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()

	// Delete expired entries from registry_cache
	_, err := s.db.Exec(`DELETE FROM registry_cache WHERE expires_at <= ?`, now)
	if err != nil {
		return fmt.Errorf("failed to cleanup registry_cache: %w", err)
	}

	// Delete expired entries from packages_metadata
	_, err = s.db.Exec(`DELETE FROM packages_metadata WHERE expires_at <= ?`, now)
	if err != nil {
		return fmt.Errorf("failed to cleanup packages_metadata: %w", err)
	}

	// Delete expired tarball metadata and remove orphaned files
	rows, err := s.db.Query(`SELECT file_path FROM tarballs WHERE expires_at <= ?`, now)
	if err != nil {
		return fmt.Errorf("failed to query expired tarballs: %w", err)
	}
	defer rows.Close() // CRITICAL: Always close rows

	var filesToDelete []string
	for rows.Next() {
		var filePath string
		if err := rows.Scan(&filePath); err != nil {
			continue
		}
		filesToDelete = append(filesToDelete, filePath)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating tarball rows: %w", err)
	}

	// Delete expired tarball metadata
	_, err = s.db.Exec(`DELETE FROM tarballs WHERE expires_at <= ?`, now)
	if err != nil {
		return fmt.Errorf("failed to cleanup tarballs: %w", err)
	}

	// Remove orphaned tarball files
	for _, filePath := range filesToDelete {
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("Warning: failed to remove orphaned tarball %s: %v\n", filePath, err)
		}
	}

	return nil
}
