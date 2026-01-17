package cli

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	npmproxy "github.com/pkgems/npm-cache-proxy/proxy"
	"github.com/pkgems/npm-cache-proxy/internal/storage"
)

const usage = `ncp - A fast npm cache proxy that supports Redis and SQLite storage backends.

Usage:
  ncp [options]
  ncp list [options]
  ncp purge [options]

Commands:
  ncp              Start the npm cache proxy server
  ncp list         List all cached paths
  ncp purge        Purge all cached paths

Options:
  -h, --help                          Show this help message
  
  Common Options:
  --listen <address>                  Address to listen on [default: localhost:8080]
  --upstream <address>                Upstream registry address [default: https://registry.npmjs.org]
  --silent                            Disable logging
  --dev                               Use development logger format (human-readable)
  --cache-limit <count>               Cached packages count limit [default: 0]
  --cache-ttl <timeout>               Cache expiration timeout in seconds [default: 3600]
  --storage-backend <backend>         Storage backend (redis or sqlite) [default: sqlite]
  
  Redis Options:
  --redis-address <address>          Redis server address [default: localhost:6379]
  --redis-database <db>               Redis database number [default: 0]
  --redis-password <password>         Redis password
  --redis-prefix <prefix>             Redis keys prefix [default: ncp-]
  
  SQLite Options:
  --sqlite-db-path <path>             SQLite database file path [default: ./data/cache.db]
  --package-dir <dir>                 Directory for package storage [default: ./data/packages]
  --sqlite-checkpoint-interval <dur>   WAL checkpoint interval [default: 5m]
  --sqlite-cleanup-interval <dur>     Expired entry cleanup interval [default: 1h]
  --sqlite-wal-size-alert-mb <mb>     Alert threshold for WAL file size in MB [default: 50]
  --sqlite-max-open-conns <n>         Maximum open connections [default: 25]
  --sqlite-max-idle-conns <n>         Maximum idle connections [default: 5]
  --sqlite-conn-max-lifetime <dur>    Connection max lifetime [default: 5m]

Environment Variables:
  Common:
  LISTEN_ADDRESS                      Address to listen on
  UPSTREAM_ADDRESS                    Upstream registry address
  SILENT                              Disable logging (set to "1" or "true")
  DEV                                 Use development logger format (set to "1" or "true")
  CACHE_LIMIT                         Cached packages count limit
  CACHE_TTL                           Cache expiration timeout in seconds
  STORAGE_BACKEND                     Storage backend (redis or sqlite)
  
  Redis:
  REDIS_ADDRESS                       Redis server address
  REDIS_DATABASE                      Redis database number
  REDIS_PASSWORD                      Redis password
  REDIS_PREFIX                        Redis keys prefix
  
  SQLite:
  SQLITE_DB_PATH                      SQLite database file path
  PACKAGE_DIR                         Directory for package storage
  SQLITE_CHECKPOINT_INTERVAL          WAL checkpoint interval
  SQLITE_CLEANUP_INTERVAL             Expired entry cleanup interval
  SQLITE_WAL_SIZE_ALERT_MB            Alert threshold for WAL file size in MB
  SQLITE_MAX_OPEN_CONNS               Maximum open connections
  SQLITE_MAX_IDLE_CONNS               Maximum idle connections
  SQLITE_CONN_MAX_LIFETIME            Connection max lifetime
`

type Options struct {
	// Command flags
	List   bool
	Purge  bool
	
	// Server options
	ListenAddress   string
	UpstreamAddress string
	Silent          bool
	Dev             bool
	CacheLimit      string
	CacheTTL        int
	
	// Storage options
	StorageBackend           string
	PackageDir               string
	RedisAddress             string
	RedisDatabase            int
	RedisPassword            string
	RedisPrefix              string
	SQLiteDBPath             string
	SQLiteCheckpointInterval time.Duration
	SQLiteCleanupInterval    time.Duration
	SQLiteWALSizeAlertMB     int
	SQLiteMaxOpenConns       int
	SQLiteMaxIdleConns       int
	SQLiteConnMaxLifetime    time.Duration
}

func parseOptions(args map[string]interface{}) Options {
	opts := Options{
		List:   getBool(args, "list", false),
		Purge:  getBool(args, "purge", false),
		Silent: getBool(args, "--silent", false),
		Dev:    getBool(args, "--dev", false),
	}
	
	// Parse string options with environment variable fallback
	opts.ListenAddress = getStringArg(args, "--listen", "LISTEN_ADDRESS", "localhost:8080")
	opts.UpstreamAddress = getStringArg(args, "--upstream", "UPSTREAM_ADDRESS", "https://registry.npmjs.org")
	opts.CacheLimit = getStringArg(args, "--cache-limit", "CACHE_LIMIT", "0")
	opts.StorageBackend = getStringArg(args, "--storage-backend", "STORAGE_BACKEND", "sqlite")
	opts.PackageDir = getStringArg(args, "--package-dir", "PACKAGE_DIR", "./data/packages")
	opts.RedisAddress = getStringArg(args, "--redis-address", "REDIS_ADDRESS", "localhost:6379")
	opts.RedisPassword = getStringArg(args, "--redis-password", "REDIS_PASSWORD", "")
	opts.RedisPrefix = getStringArg(args, "--redis-prefix", "REDIS_PREFIX", "ncp-")
	opts.SQLiteDBPath = getStringArg(args, "--sqlite-db-path", "SQLITE_DB_PATH", "./data/cache.db")
	
	// Parse integer options
	opts.CacheTTL = getIntArg(args, "--cache-ttl", "CACHE_TTL", 3600)
	opts.RedisDatabase = getIntArg(args, "--redis-database", "REDIS_DATABASE", 0)
	opts.SQLiteWALSizeAlertMB = getIntArg(args, "--sqlite-wal-size-alert-mb", "SQLITE_WAL_SIZE_ALERT_MB", 50)
	opts.SQLiteMaxOpenConns = getIntArg(args, "--sqlite-max-open-conns", "SQLITE_MAX_OPEN_CONNS", 25)
	opts.SQLiteMaxIdleConns = getIntArg(args, "--sqlite-max-idle-conns", "SQLITE_MAX_IDLE_CONNS", 5)
	
	// Parse duration options
	opts.SQLiteCheckpointInterval = getDurationArg(args, "--sqlite-checkpoint-interval", "SQLITE_CHECKPOINT_INTERVAL", 5*time.Minute)
	opts.SQLiteCleanupInterval = getDurationArg(args, "--sqlite-cleanup-interval", "SQLITE_CLEANUP_INTERVAL", 1*time.Hour)
	opts.SQLiteConnMaxLifetime = getDurationArg(args, "--sqlite-conn-max-lifetime", "SQLITE_CONN_MAX_LIFETIME", 5*time.Minute)
	
	return opts
}

func getStringArg(args map[string]interface{}, flag, envVar, defaultValue string) string {
	if val, ok := args[flag]; ok && val != nil {
		if str, ok := val.(string); ok && str != "" {
			return str
		}
	}
	return getEnvString(envVar, defaultValue)
}

func getIntArg(args map[string]interface{}, flag, envVar string, defaultValue int) int {
	if val, ok := args[flag]; ok && val != nil {
		if str, ok := val.(string); ok && str != "" {
			if parsed, err := strconv.Atoi(str); err == nil {
				return parsed
			}
		}
	}
	return getEnvInt(envVar, strconv.Itoa(defaultValue))
}

func getBool(args map[string]interface{}, key string, defaultValue bool) bool {
	if val, ok := args[key]; ok {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	// Also check environment variable for silent and dev flags
	if key == "--silent" {
		return getEnvBool("SILENT", "0")
	}
	if key == "--dev" {
		return getEnvBool("DEV", "0")
	}
	return defaultValue
}

func getDurationArg(args map[string]interface{}, flag, envVar string, defaultValue time.Duration) time.Duration {
	if val, ok := args[flag]; ok && val != nil {
		if str, ok := val.(string); ok && str != "" {
			if parsed, err := time.ParseDuration(str); err == nil {
				return parsed
			}
		}
	}
	envVal := getEnvString(envVar, defaultValue.String())
	if parsed, err := time.ParseDuration(envVal); err == nil {
		return parsed
	}
	return defaultValue
}

func getProxy(opts Options) (*npmproxy.Proxy, storage.Storage, error) {
	// Create storage backend using factory
	backendType := storage.BackendType(opts.StorageBackend)
	if backendType != storage.BackendRedis && backendType != storage.BackendSQLite {
		return nil, nil, fmt.Errorf("invalid storage backend: %s (must be 'redis' or 'sqlite')", opts.StorageBackend)
	}

	config := storage.Config{
		Backend:                 backendType,
		RedisAddress:            opts.RedisAddress,
		RedisPassword:           opts.RedisPassword,
		RedisDatabase:           opts.RedisDatabase,
		SQLiteDBPath:            opts.SQLiteDBPath,
		SQLitePackageDir:        opts.PackageDir,
		SQLiteCheckpointInterval: opts.SQLiteCheckpointInterval,
		SQLiteCleanupInterval:    opts.SQLiteCleanupInterval,
		SQLiteWALSizeAlertMB:     opts.SQLiteWALSizeAlertMB,
		SQLiteMaxOpenConns:       opts.SQLiteMaxOpenConns,
		SQLiteMaxIdleConns:       opts.SQLiteMaxIdleConns,
		SQLiteConnMaxLifetime:    opts.SQLiteConnMaxLifetime,
	}

	storageBackend, err := storage.NewStorage(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create storage backend: %w", err)
	}

	// Create adapter to convert storage.Storage to proxy.Database interface
	adapter := &storageAdapter{storage: storageBackend}

	// Optimize HTTP client for better concurrency and performance
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false, // Allow gzip compression
	}
	
	return &npmproxy.Proxy{
		Database: adapter,
		HttpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Minute, // Timeout for large downloads
		},
	}, storageBackend, nil
}

// storageAdapter adapts storage.Storage to proxy.Database interface
type storageAdapter struct {
	storage storage.Storage
}

func (a *storageAdapter) Get(key string) (string, error) {
	return a.storage.Get(key)
}

func (a *storageAdapter) Set(key string, value string, ttl time.Duration) error {
	return a.storage.Set(key, value, ttl)
}

func (a *storageAdapter) Delete(key string) error {
	return a.storage.Delete(key)
}

func (a *storageAdapter) Keys(prefix string) ([]string, error) {
	return a.storage.Keys(prefix)
}

func (a *storageAdapter) RefreshExpiration(key string, expiration time.Duration) error {
	return a.storage.RefreshExpiration(key, expiration)
}

func (a *storageAdapter) Health() error {
	return a.storage.Health()
}

// Run starts the CLI
func Run() {
	args, err := docopt.ParseArgs(usage, os.Args[1:], "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing arguments: %v\n", err)
		os.Exit(1)
	}

	opts := parseOptions(args)

	if opts.List {
		runList(opts)
	} else if opts.Purge {
		runPurge(opts)
	} else {
		runServer(opts)
	}
}

// isDatabaseNotInitializedError checks if the error is related to database initialization
func isDatabaseNotInitializedError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Check for common SQLite initialization errors
	return strings.Contains(errStr, "failed to configure pragmas") ||
		strings.Contains(errStr, "failed to create storage backend") ||
		strings.Contains(errStr, "failed to open database") ||
		strings.Contains(errStr, "no such file or directory") ||
		strings.Contains(errStr, "unable to open database file")
}
