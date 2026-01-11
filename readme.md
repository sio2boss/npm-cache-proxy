<div align="center">
  <img width="450" src="./logo.png"> 

  <h1>npm-cache-proxy</h1>

  <a href="https://hub.docker.com/r/pkgems/npm-cache-proxy/tags">
    <img src="https://img.shields.io/github/release/pkgems/npm-cache-proxy.svg" alt="Current Release" />
  </a>
  <a href="https://hub.docker.com/r/pkgems/npm-cache-proxy/builds">
    <img src="https://img.shields.io/docker/cloud/build/pkgems/npm-cache-proxy.svg" alt="CI Build">
  </a>
  <a href="https://github.com/pkgems/npm-cache-proxy/blob/master/license">
    <img src="https://img.shields.io/github/license/pkgems/npm-cache-proxy.svg" alt="Licence">
  </a>
</div>

<br />
<br />

## Introduction

#### ⚡️ Performance
NCP is a tiny but very fast caching proxy written in Go. It uses Redis for data storage, which in combination with the speed of Go makes it incredibly fast. NCP is well-optimized and can be run on almost any platform, so if you have a Raspberry Pi, you can install NCP as your local cache there.

#### ✨ Modularity
NCP is modular. Now it has only one database adapter which is Redis. If you need support for any other database, feel free to open an issue or implement it [on your own](https://github.com/pkgems/npm-cache-proxy/blob/7c8b90ff6ba0656f60e3de915b9fb4eaabfb467b/proxy/proxy.go#L29) and then open a pull request (_bonus points_).

#### 💡 Simplicity
NCP is very simple. It just proxies requests to an upstream registry, caches response and returns cached response for next requests to the same package. Cached data are stored in Redis with an original request URL as a key.


<br />


## Installation
NCP binaries for different paltforms can be downloaded can be downloaded on the [Releases](https://github.com/pkgems/npm-cache-proxy/releases) page. Also, Docker image is provided on [Docker Hub](https://cloud.docker.com/u/pkgems/repository/docker/pkgems/npm-cache-proxy).

#### 💫 Quick Start
The quickies way to get started with NCP is to use Docker.

```bash
# Run proxy inside of docker container in background
docker run -e REDIS_ADDRESS=host.docker.internal:6379 -p 8080:8080 -it -d pkgems/npm-cache-proxy

# Run locally with Redis (default)
ncp --redis-address localhost:6379

# Run locally with SQLite and human-readable logging
ncp --dev 

# Configure npm to use caching proxy as registry
npm config set registry http://localhost:8080
```

<br />

## CLI
NCP provides command line interface for interaction with a cached data.

<details>

```
$ ./ncp --help
ncp - A fast npm cache proxy that supports Redis and SQLite storage backends.

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
```

</details>

#### `ncp`
Start NCP server with default options (SQLite)

#### `ncp list`
List cached url paths.

#### `ncp purge`
Purge cached url paths.

<br />


## Benchmark
Benchmark is run on Macbook Pro 15″ 2017, Intel Core i7-7700HQ.

#### 1️⃣ 1 process

```bash
# GOMAXPROCS=1 ncp --silent

$ go-wrk -c 100 -d 6 http://localhost:8080/tiny-tarball
Running 6s test @ http://localhost:8080/tiny-tarball
  100 goroutine(s) running concurrently

70755 requests in 5.998378587s, 91.16MB read

Requests/sec:		11795.69
Transfer/sec:		15.20MB
Avg Req Time:		8.477674ms
Fastest Request:	947.743µs
Slowest Request:	815.787409ms
Number of Errors:	0
```

#### ♾ unlimited processes

```bash
# ncp --silent

$ go-wrk -c 100 -d 6 http://localhost:8080/tiny-tarball
Running 6s test @ http://localhost:8080/tiny-tarball
  100 goroutine(s) running concurrently

115674 requests in 5.98485984s, 149.04MB read

Requests/sec:		19327.77
Transfer/sec:		24.90MB
Avg Req Time:		5.173902ms
Fastest Request:	273.015µs
Slowest Request:	34.777963ms
Number of Errors:	0
```


<br />


## Programmatic Usage
NCP provides `proxy` go package that can be used programmatically. Docs are available on [godoc.org](https://godoc.org/github.com/pkgems/npm-cache-proxy/proxy).

#### 🤖 Example
```golang
package main

import (
	"net/http"
	"time"

	npmproxy "github.com/pkgems/npm-cache-proxy/proxy"
	"github.com/go-redis/redis"
)

func main() {
	// create proxy
	proxy := npmproxy.Proxy{
		// use redis as database
		Database: npmproxy.DatabaseRedis{
			// see github.com/go-redis/redis
			Client: redis.NewClient(&redis.Options{
				Addr:     "localhost:6379",
			}),
		},

		// reuse connections
		HttpClient: &http.Client{},
	}

	// create and start server
	proxy.Server(npmproxy.ServerOptions{
		ListenAddress: "localhost:8080",

		// allow fetching options dynamically on each request
		GetOptions: func() (npmproxy.Options, error) {
			return npmproxy.Options{
				DatabasePrefix:     "ncp-",
				DatabaseExpiration: 1 * time.Hour,
				UpstreamAddress:    "https://registry.npmjs.org",
			}, nil
		},
	}).ListenAndServe()
}
```


<br />


## License
[MIT](./license)
