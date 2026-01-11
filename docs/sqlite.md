# npm-cache-proxy: SQLite/libSQL Storage Backend Design Document

## Executive Summary

This document outlines the design for adding SQLite/libSQL as an optional storage backend to npm-cache-proxy, while maintaining Redis as the existing default option. This gives users flexibility to choose between:

- **Redis**: External server, mature, excellent for distributed deployments
- **SQLite/libSQL**: Embedded database, single binary deployment, simpler operations

## Goals

1. Maintain 100% backward compatibility with existing Redis implementation
2. Add SQLite/libSQL as an alternative storage backend
3. Create a clean abstraction layer for potential future backends
4. Preserve single-executable deployment option (no external dependencies when using SQLite)
5. Ensure feature parity between Redis and SQLite implementations

## Architecture Overview

### Storage Abstraction Layer

Introduce a storage interface that abstracts cache operations from the underlying storage mechanism. Both Redis and SQLite implementations will satisfy this interface.

**Key Components:**
- **Storage Interface**: Defines all cache operations (get, set, delete, list, etc.)
- **Redis Implementation**: Wraps existing Redis code to satisfy the interface
- **SQLite Implementation**: New implementation using libSQL
- **Factory Pattern**: Creates appropriate storage backend based on configuration
- **Cache Layer**: Uses storage interface, agnostic to backend

### Directory Structure

```
internal/
├── storage/
│   ├── storage.go         # Interface definition
│   ├── redis.go           # Redis implementation
│   ├── sqlite.go          # SQLite/libSQL implementation
│   └── factory.go         # Factory for backend selection
├── cache/
│   ├── cache.go           # Cache logic using storage interface
│   └── tarball.go         # Tarball management
```

## Storage Interface Design

### Core Operations Required

**Package Metadata Operations:**
- Get package metadata by name and version
- Set package metadata with TTL
- Delete package metadata

**Tarball Metadata Operations:**
- Get tarball metadata (file path, size, checksum)
- Set tarball metadata with TTL
- Delete tarball metadata

**Generic Key-Value Operations:**
- Get arbitrary cached data
- Set arbitrary cached data with TTL
- Delete cached data

**Bulk Operations:**
- List packages by prefix
- Cleanup expired entries (SQLite only - Redis handles via TTL)

**Lifecycle Operations:**
- Initialize/connect
- Health check/ping
- Graceful shutdown/close

### Design Considerations

**TTL Handling:**
- Redis: Native TTL support via EXPIRE
- SQLite: Store expiration timestamp, filter in queries, periodic cleanup job

**Data Types:**
- Redis: Strings/bytes directly
- SQLite: Store as TEXT/BLOB in tables

**Concurrency:**
- Redis: Handles concurrent access natively
- SQLite: WAL mode for concurrent reads, single writer

## SQLite/libSQL Implementation Details

### Why libSQL Over Vanilla SQLite

**Advantages of libSQL:**
- Better write concurrency than standard SQLite
- Active development by Turso team
- SQLite-compatible (easy migration path if needed)
- Future-proof for potential distributed features (embedded replicas)
- Better performance characteristics

### Database Schema

**Tables Required:**

**packages_metadata:**
- package_name (TEXT)
- version (TEXT)
- metadata (TEXT/JSON) - serialized package.json
- created_at (INTEGER) - Unix timestamp
- expires_at (INTEGER) - Unix timestamp for TTL simulation
- PRIMARY KEY (package_name, version)
- INDEX on expires_at for cleanup queries

**tarballs:**
- package_name (TEXT)
- version (TEXT)
- file_path (TEXT) - path to .tgz on filesystem
- size (INTEGER)
- checksum (TEXT)
- created_at (INTEGER)
- expires_at (INTEGER)
- PRIMARY KEY (package_name, version)
- INDEX on expires_at

**registry_cache:**
- key (TEXT PRIMARY KEY)
- value (TEXT/BLOB)
- created_at (INTEGER)
- expires_at (INTEGER)
- INDEX on expires_at

### Storage Strategy for Tarballs

**Store on Filesystem, Not in Database:**
- Actual .tgz files remain in filesystem directory structure
- SQLite stores only metadata and file path reference
- Prevents database bloat
- Better performance for large binary files
- Consistent with how Redis implementation likely works

### SQLite Configuration (Pragmas)

**Required Settings:**
- `journal_mode=WAL` - Write-Ahead Logging for concurrency
- `synchronous=NORMAL` - Balance safety and performance
- `cache_size=-64000` - 64MB page cache
- `temp_store=MEMORY` - Temp tables in memory
- `mmap_size=268435456` - 256MB memory-mapped I/O

### Critical: WAL Checkpoint Management

**The Problem (from Turso article):**
Unclosed query result sets in Go prevent WAL checkpoint operations. This causes the WAL file to grow indefinitely (potentially 10-100x normal size), consuming massive disk space while the application continues working normally.

**Root Cause:**
SQLite treats unclosed `rows` objects as ongoing read transactions. Checkpoints cannot advance past active read transactions, so the WAL file cannot be truncated.

**Prevention Strategy:**

1. **Mandatory Query Cleanup Pattern:**
   - ALWAYS defer `rows.Close()` immediately after checking query error
   - Never return from a function without closing rows
   - Use linting/code review to enforce this pattern

2. **Automated Checkpoint Management:**
   - Periodic checkpoint job (every 5 minutes recommended)
   - Execute `PRAGMA wal_checkpoint(TRUNCATE)` on schedule
   - Run checkpoint during graceful shutdown
   - Independent goroutine managing checkpoint lifecycle

3. **Monitoring:**
   - Track WAL file size vs main database file size
   - Alert if WAL grows disproportionately (e.g., >5x main DB)
   - Log checkpoint operations and results
   - Include in health check metrics

4. **Testing:**
   - Specific tests for query leak detection
   - Load tests monitoring WAL growth
   - Verify checkpoints occur under concurrent load

**Implementation Notes:**
- Checkpoint manager runs in separate goroutine
- Ticker-based periodic execution
- Graceful shutdown includes final checkpoint
- Error logging for failed checkpoints (don't crash)

### Expiration and Cleanup

**TTL Simulation:**
Since SQLite doesn't have native TTL:
- Store `expires_at` timestamp with each entry
- Filter expired entries in all SELECT queries (WHERE expires_at > current_time)
- Return NULL/empty for expired entries (mimics Redis behavior)

**Cleanup Job:**
- Periodic background job (e.g., every hour)
- DELETE expired entries from all tables
- Remove orphaned tarball files from filesystem
- Run in separate goroutine
- Include in checkpoint manager or separate

**Query Pattern:**
All SELECT queries MUST check expiration:
```sql
SELECT * FROM packages_metadata 
WHERE package_name = ? AND version = ? 
AND expires_at > ?  -- Current Unix timestamp
```

### Concurrency Model

**WAL Mode Benefits:**
- Multiple concurrent readers (no blocking)
- Single writer at a time
- Readers don't block writers (mostly)
- Writers don't block readers (mostly)

**Connection Pool Configuration:**
- Max open connections: 25 (adjust based on load)
- Max idle connections: 5
- Connection max lifetime: 5 minutes
- Single connection for embedded mode would work but pool allows better concurrency

**Write Lock Handling:**
- libSQL handles better than vanilla SQLite
- Retry logic may still be beneficial for high concurrency
- Use transactions for atomic multi-operation updates

## Configuration Design

### Configuration Options

**Storage Backend Selection:**
- CLI flag: `--storage-backend` (values: `redis`, `sqlite`)
- Environment variable: `STORAGE_BACKEND` (values: `redis`, `sqlite`)
- Default: `redis` (maintains backward compatibility)
- Config file option for more complex deployments
- **Implementation:** Add as persistent flag in `cli/main.go` alongside existing Redis flags
- **Usage:** `ncp --storage-backend sqlite` or `STORAGE_BACKEND=sqlite ncp`

**Redis Configuration (existing):**
- CLI flags: `--redis-address`, `--redis-password`, `--redis-database`, `--redis-prefix` (already implemented)
- Environment variables: `REDIS_ADDRESS`, `REDIS_PASSWORD`, `REDIS_DATABASE`, `REDIS_PREFIX`
- Connection pool settings

**SQLite Configuration (new):**
- CLI flags:
  - `--sqlite-db-path` - Path to database file (e.g., `./data/cache.db`)
  - `--sqlite-checkpoint-interval` - WAL checkpoint interval (default: 5m)
  - `--sqlite-cleanup-interval` - Expired entry cleanup interval (default: 1h)
  - `--sqlite-wal-size-alert-mb` - Alert threshold for WAL file size (default: 50)
- Environment variables:
  - `SQLITE_DB_PATH` - Path to database file (e.g., `./data/cache.db`)
  - `SQLITE_CHECKPOINT_INTERVAL` - WAL checkpoint interval (default: 5m)
  - `SQLITE_CLEANUP_INTERVAL` - Expired entry cleanup interval (default: 1h)
  - `SQLITE_WAL_SIZE_ALERT_MB` - Alert threshold for WAL file size (default: 50)
- **Implementation:** Add as persistent flags in `cli/main.go`, conditionally used when `--storage-backend=sqlite`

**Common Configuration:**
- CLI flags:
  - `--package-dir` - Directory for package storage (both backends, default: `./cache`)
  - `--cache-ttl` - Default cache TTL (both backends, already implemented)
  - `--cache-limit` - Optional size limit (already implemented)
- Environment variables:
  - `PACKAGE_DIR` - Directory for package storage (both backends, default: `./cache`)
  - `CACHE_TTL` - Default cache TTL (both backends, already implemented)
  - `CACHE_LIMIT` - Optional size limit (already implemented)
- **Implementation:** Add `--package-dir` flag in `cli/root.go` with default value `./cache`, passed to storage backend initialization

### CLI Flag Implementation Details

**Backend Selection Logic:**
The CLI will use a factory pattern to create the appropriate storage backend based on the `--storage-backend` flag:

1. **In `cli/main.go`:**
   - Add `StorageBackend` to `persistentOptions` struct
   - Add `--storage-backend` persistent flag (default: `redis`)
   - Add `PackageDir` to `persistentOptions` struct
   - Add `--package-dir` persistent flag (default: `./cache`)

2. **In `cli/utils.go` or new `cli/factory.go`:**
   - Create `getStorageBackend()` function that:
     - Reads `persistentOptions.StorageBackend`
     - If `redis`: Creates `DatabaseRedis` instance using existing Redis flags
     - If `sqlite`: Creates `DatabaseSQLite` instance using SQLite flags
     - Passes `persistentOptions.PackageDir` to both backends for package file storage

3. **In `cli/main.go` `getProxy()` function:**
   - Replace hardcoded `DatabaseRedis` with call to `getStorageBackend()`
   - Ensure tarball directory is initialized/created if it doesn't exist

**Example CLI Usage:**
```bash
# Use Redis (default)
ncp --redis-address localhost:6379 --package-dir /var/cache/npm

# Use SQLite
ncp --storage-backend sqlite --sqlite-db-path ./data/cache.db --package-dir /var/cache/npm

# Using environment variables
STORAGE_BACKEND=sqlite SQLITE_DB_PATH=./data/cache.db PACKAGE_DIR=/var/cache/npm ncp
```

**Package Directory Usage:**
- The `--package-dir` flag specifies where actual `.tgz` files are stored on the filesystem
- Both Redis and SQLite backends will use this directory for physical tarball storage
- Redis stores metadata in Redis, file paths reference this directory
- SQLite stores metadata in database, file paths reference this directory
- Directory will be created automatically if it doesn't exist (with appropriate permissions)
- Default path: `./cache` (relative to working directory)

### Migration Path

**Existing Deployments:**
- Default to Redis if no configuration provided
- Zero impact on current users

**New Deployments:**
- Can choose SQLite for simpler deployment
- Set `STORAGE_BACKEND=sqlite`
- Single binary, no external dependencies

## Implementation Phases

### Phase 1: Interface and Redis Wrapper (1-2 days)

**Objectives:**
- Define storage interface
- Wrap existing Redis code to satisfy interface
- Update cache layer to use interface
- Ensure all existing tests pass
- Zero functionality change

**Deliverables:**
- `storage/storage.go` with complete interface
- `storage/redis.go` implementing interface
- `storage/factory.go` for backend creation
- Updated cache layer using interface
- All existing tests passing

### Phase 2: SQLite Implementation (3-5 days)

**Objectives:**
- Implement SQLite storage backend
- Database schema and migrations
- WAL checkpoint manager
- Expiration and cleanup jobs
- Query patterns with proper resource cleanup

**Deliverables:**
- `storage/sqlite.go` implementing interface
- Schema initialization code
- Checkpoint manager goroutine
- Cleanup job goroutine
- Initial unit tests

### Phase 3: Testing and Validation (2-3 days)

**Objectives:**
- Comprehensive unit tests for SQLite backend
- Integration tests for both backends
- Performance testing and benchmarking
- Query leak detection tests
- Load testing with concurrent requests

**Deliverables:**
- Full test suite for SQLite implementation
- Benchmark comparisons (Redis vs SQLite)
- Load test results
- Query leak verification tests

### Phase 4: Configuration and Documentation (1-2 days)

**Objectives:**
- Configuration management
- CLI flag implementation for backend selection (`--storage-backend`)
- CLI flag implementation for package directory (`--package-dir`)
- Factory implementation in CLI (`cli/utils.go` or `cli/factory.go`)
- Update `cli/main.go` to use factory pattern for backend selection
- Environment variable support for all new options
- User documentation
- Operational runbooks
- Migration guide

**Deliverables:**
- Complete configuration system with CLI flags
- `--storage-backend` flag (redis/sqlite selection)
- `--package-dir` flag (default: `./cache`)
- SQLite-specific CLI flags (`--sqlite-db-path`, `--sqlite-checkpoint-interval`, etc.)
- Factory function for backend creation
- Updated `getProxy()` function in `cli/main.go`
- README updates with new CLI flags
- Operations documentation
- Deployment examples for both backends

### Phase 5: Monitoring and Observability (1-2 days)

**Objectives:**
- Health check endpoints
- Metrics for both backends
- WAL file size monitoring
- Cache hit/miss rates
- Performance metrics

**Deliverables:**
- Health check implementation
- Prometheus/metrics integration
- Logging standards
- Alerting guidelines

## Testing Strategy

### Unit Tests

**Storage Interface Compliance:**
- Test suite that runs against interface
- Both implementations must pass identical tests
- Verify TTL behavior matches
- Test edge cases (empty results, errors, etc.)

**SQLite-Specific Tests:**
- Schema creation and migrations
- Query cleanup (no leaks)
- WAL checkpoint operations
- Expiration filtering
- Cleanup job execution

**Redis-Specific Tests:**
- Maintain existing test coverage
- Ensure wrapper doesn't break functionality

### Integration Tests

**End-to-End Scenarios:**
- Package caching workflow
- Tarball storage and retrieval
- Cache expiration
- Concurrent access patterns

**Backend Switching:**
- Verify application works with both backends
- Configuration-driven selection
- Factory pattern correctness

### Performance Tests

**Benchmarks:**
- Get/Set operations (both backends)
- Concurrent read performance
- Concurrent write performance
- Large dataset handling
- Memory usage comparison

**Load Testing:**
- Simulate realistic npm traffic patterns
- Sustained concurrent requests
- Cache hit/miss ratios
- Resource utilization (CPU, memory, disk)

### Query Leak Detection Tests

**Critical for SQLite:**
- Create test that runs many queries
- Monitor WAL file size growth
- Verify cleanup occurs properly
- Test checkpoint effectiveness
- Ensure no memory leaks from unclosed rows

## Operational Considerations

### Deployment Scenarios

**Single Server (SQLite Recommended):**
- Single binary deployment
- No external dependencies
- Simpler operations
- Local persistence

**Distributed/Multi-Server (Redis Recommended):**
- Shared cache across instances
- Centralized cache management
- Better for load balancing scenarios
- Network dependency acceptable

**Development Environment:**
- SQLite ideal for local development
- No Docker/external services needed
- Faster dev setup
- Easy testing

### Monitoring Requirements

**Both Backends:**
- Cache hit/miss rates
- Request latency
- Error rates
- Memory usage
- Disk usage

**SQLite-Specific:**
- WAL file size
- Main database file size
- Checkpoint frequency and duration
- Query performance
- Cleanup job execution

**Redis-Specific:**
- Connection pool stats
- Network latency to Redis
- Redis server health
- Memory usage on Redis server

### Backup and Recovery

**SQLite:**
- Simple file-based backups
- Copy database file when idle or use backup API
- Point-in-time recovery via file snapshots
- Consider WAL file in backup strategy

**Redis:**
- Existing Redis backup strategy (RDB/AOF)
- Separate from application concerns
- Follow Redis best practices

### Maintenance Tasks

**SQLite:**
- Periodic VACUUM to reclaim space (monthly/quarterly)
- WAL checkpoint monitoring
- Orphaned tarball cleanup
- Database file size monitoring

**Redis:**
- Follow existing Redis maintenance procedures
- Key expiration monitoring
- Memory management

## Migration Considerations

### Backward Compatibility

**Guarantee:**
- Existing Redis deployments work without changes
- Default configuration remains Redis
- No breaking changes to existing API
- All environment variables remain valid




## Success Criteria

- Storage interface fully defined
- Redis implementation maintains all existing functionality
- SQLite implementation achieves feature parity
- Both backends pass identical test suite
- Configuration-driven backend selection
- WAL checkpoint automation implemented
- Query cleanup patterns enforced
- Basic monitoring and health checks
- Comprehensive performance benchmarks
- Production load testing results
- Detailed operational documentation
- Metrics and observability integration

## Questions for Development Team

### Implementation Questions

1. **Existing Key Structure:** How are cache keys currently structured in Redis? (e.g., `package:name:version`)
2. **TTL Patterns:** What are common TTL values used? Are they consistent or variable?
3. **Tarball Storage:** How are tarballs currently stored in filesystem? Path structure?
4. **Concurrency:** What are typical concurrent request patterns in production?
5. **Cache Size:** What are typical cache sizes (number of packages, disk space)?

### Testing Questions

1. **Existing Tests:** What test coverage exists for cache operations?
2. **Integration Tests:** Are there existing integration tests we can extend?
3. **Load Testing:** Do you have load testing infrastructure to reuse?
4. **Performance Baselines:** What are current performance baselines with Redis?

### Operational Questions

1. **Monitoring:** What monitoring/metrics system is currently used?
2. **Deployment:** How is npm-cache-proxy currently deployed? (Containers, VMs, etc.)
3. **Scaling:** Do you run single instance or multiple instances?
4. **Backup:** What's the current backup strategy for Redis?

## Recommended Reading

### Critical Resource

**Turso Blog Article:** "Something you probably want to know about if you're using SQLite in Golang"
- URL: https://turso.tech/blog/something-you-probably-want-to-know-about-if-youre-using-sqlite-in-golang-72547ad625f1
- **Must read for any developer working on SQLite implementation**
- Explains the query leak / WAL checkpoint issue in detail
- Provides code examples and solutions

### Additional Resources

**SQLite Documentation:**
- Write-Ahead Logging: https://www.sqlite.org/wal.html
- Pragma Statements: https://www.sqlite.org/pragma.html
- Performance Tuning: https://www.sqlite.org/optoverview.html

**libSQL Documentation:**
- GitHub: https://github.com/tursodatabase/libsql
- Go Client: https://github.com/tursodatabase/turso


## Conclusion

This design provides a clean, maintainable path to add SQLite/libSQL as an optional storage backend while preserving all existing Redis functionality. The interface-based approach ensures both implementations are first-class citizens.

The critical focus on query cleanup and WAL checkpoint management ensures the SQLite implementation will be production-ready and won't suffer from the disk bloat issues that commonly affect SQLite deployments.

Success depends on strict adherence to the query cleanup patterns, comprehensive testing, and proper monitoring of the SQLite backend in production environments.