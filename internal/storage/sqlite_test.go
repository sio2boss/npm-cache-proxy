package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSQLite(t *testing.T) *SQLiteStorage {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSQLiteStorage(SQLiteConfig{
		DBPath:              filepath.Join(dir, "cache.db"),
		PackageDir:          filepath.Join(dir, "packages"),
		CheckpointInterval:  time.Hour,
		CleanupInterval:     time.Hour,
		AccessFlushInterval: time.Hour,
		MaxOpenConns:        1,
		MaxIdleConns:        1,
	})
	if err != nil {
		t.Fatalf("NewSQLiteStorage: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestTarballPathFromKey(t *testing.T) {
	path, err := tarballPathFromKey("custom-/left-pad/-/left-pad-1.0.0.tgz:tarball")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/left-pad/-/left-pad-1.0.0.tgz" {
		t.Fatalf("path = %q", path)
	}

	if _, err := tarballPathFromKey("no-slash:tarball"); err == nil {
		t.Fatal("expected error for key without a path")
	}
}

func TestStartupClearsExpiredEntries(t *testing.T) {
	dir := t.TempDir()
	cfg := SQLiteConfig{
		DBPath:              filepath.Join(dir, "cache.db"),
		PackageDir:          filepath.Join(dir, "packages"),
		CheckpointInterval:  time.Hour,
		CleanupInterval:     time.Hour,
		AccessFlushInterval: time.Hour,
		MaxOpenConns:        1,
		MaxIdleConns:        1,
	}

	s, err := NewSQLiteStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}

	expiredKey := "ncp-/old"
	if err := s.Set(expiredKey, `{"name":"old"}`, time.Hour); err != nil {
		t.Fatal(err)
	}

	expiredFile := filepath.Join(cfg.PackageDir, "old.tgz")
	if err := os.WriteFile(TarballPartialPath(expiredFile), []byte("old-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	expiredTarball := "custom-/old/-/old-1.0.0.tgz:tarball"
	if err := s.Set(expiredTarball, expiredFile, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(expiredFile); err != nil {
		t.Fatalf("published tarball missing: %v", err)
	}
	if _, err := os.Stat(TarballPartialPath(expiredFile)); !os.IsNotExist(err) {
		t.Fatalf("partial file still present: %v", err)
	}

	if _, err := s.db.Exec(`UPDATE registry_cache SET expires_at = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tarballs SET expires_at = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO packages_metadata (package_name, version, metadata, created_at, expires_at) VALUES ('old', '1.0.0', '{}', 0, 0)`); err != nil {
		t.Fatal(err)
	}

	liveKey := "ncp-/live"
	if err := s.Set(liveKey, `{"name":"live"}`, time.Hour); err != nil {
		t.Fatal(err)
	}
	liveFile := filepath.Join(cfg.PackageDir, "live.tgz")
	if err := os.WriteFile(TarballPartialPath(liveFile), []byte("live-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	liveTarball := "other-/live/-/live-2.0.0.tgz:tarball"
	if err := s.Set(liveTarball, liveFile, time.Hour); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour).Unix()
	if _, err := s.db.Exec(`INSERT INTO packages_metadata (package_name, version, metadata, created_at, expires_at) VALUES ('live', '2.0.0', '{}', ?, ?)`, later, later); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	if _, err := reopened.Get(expiredKey); err == nil || !strings.Contains(err.Error(), "key not found") {
		t.Fatalf("expired registry key still present: %v", err)
	}
	if _, err := reopened.Get(expiredTarball); err == nil || !strings.Contains(err.Error(), "key not found") {
		t.Fatalf("expired tarball key still present: %v", err)
	}
	if _, err := os.Stat(expiredFile); !os.IsNotExist(err) {
		t.Fatalf("expired tarball file still on disk: %v", err)
	}

	got, err := reopened.Get(liveKey)
	if err != nil || got != `{"name":"live"}` {
		t.Fatalf("live registry key = %q, %v", got, err)
	}
	got, err = reopened.Get(liveTarball)
	if err != nil || got != liveFile {
		t.Fatalf("live tarball = %q, %v", got, err)
	}
	if _, err := os.Stat(liveFile); err != nil {
		t.Fatalf("live tarball file missing: %v", err)
	}

	var expiredMeta, liveMeta int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM packages_metadata WHERE package_name = 'old'`).Scan(&expiredMeta); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM packages_metadata WHERE package_name = 'live'`).Scan(&liveMeta); err != nil {
		t.Fatal(err)
	}
	if expiredMeta != 0 || liveMeta != 1 {
		t.Fatalf("packages_metadata expired=%d live=%d", expiredMeta, liveMeta)
	}
}

func TestFlushUsesConfiguredTTL(t *testing.T) {
	s := newTestSQLite(t)

	key := "ncp-/pkg"
	if err := s.Set(key, "body", time.Hour); err != nil {
		t.Fatal(err)
	}
	s.RefreshExpiration(key, 2*time.Hour)
	before := time.Now().Unix()
	if err := s.flushAccessTimes(); err != nil {
		t.Fatal(err)
	}

	var expiresAt int64
	if err := s.db.QueryRow(`SELECT expires_at FROM registry_cache WHERE key = ?`, key).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	if expiresAt < before+2*3600-2 || expiresAt > before+2*3600+2 {
		t.Fatalf("expires_at = %d, want about %d", expiresAt, before+2*3600)
	}
	if _, ok := s.accessTimes.Load(key); ok {
		t.Fatal("flushed access record was left in memory")
	}

	s.RefreshExpiration(key, time.Hour)
	if _, ok := s.accessTimes.Load(key); !ok {
		t.Fatal("access recorded after the flush should stay pending")
	}

	tarballKey := "prefix-/left-pad/-/left-pad-1.2.3.tgz:tarball"
	filePath := filepath.Join(s.packageDir, "left-pad.tgz")
	if err := os.WriteFile(filePath, []byte("tar"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(tarballKey, filePath, time.Minute); err != nil {
		t.Fatal(err)
	}
	s.RefreshExpiration(tarballKey, 3*time.Hour)
	before = time.Now().Unix()
	if err := s.flushAccessTimes(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT expires_at FROM tarballs WHERE package_name = ? AND version = ?`, "left-pad", "1.2.3").Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	if expiresAt < before+3*3600-2 || expiresAt > before+3*3600+2 {
		t.Fatalf("tarball expires_at = %d, want about %d", expiresAt, before+3*3600)
	}
}

func TestCleanupKeepsKeyWithPendingRefresh(t *testing.T) {
	s := newTestSQLite(t)
	key := "ncp-/pkg"
	if err := s.Set(key, "body", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE registry_cache SET expires_at = 1 WHERE key = ?`, key); err != nil {
		t.Fatal(err)
	}

	s.RefreshExpiration(key, time.Hour)
	if err := s.cleanupExpired(); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(key)
	if err != nil || got != "body" {
		t.Fatalf("pending refresh was discarded: %q %v", got, err)
	}
}

func TestCompareAndDeleteLeavesNewerAccess(t *testing.T) {
	var s SQLiteStorage
	key := "ncp-/pkg"
	older := accessRecord{at: time.Now().Unix(), ttl: 3600}
	newer := accessRecord{at: older.at + 30, ttl: 7200}
	s.accessTimes.Store(key, newer)
	if s.accessTimes.CompareAndDelete(key, older) {
		t.Fatal("older record deleted a newer access")
	}
	got, ok := s.accessTimes.Load(key)
	if !ok || got != newer {
		t.Fatalf("access record = %#v, ok=%v", got, ok)
	}
	if s.accessTimes.CompareAndDelete(key, newer) == false {
		t.Fatal("matching record should be removed")
	}
}
