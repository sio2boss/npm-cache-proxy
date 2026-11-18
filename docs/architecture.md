# Request path

These are the diagrams from the SQLite performance plan. They show the intended cache path: metadata in memory, SQLite as the durable index, tarball bytes streamed from disk.

The running `ncp` binary still leaves the L1 wrapper off unless `EnableL1Cache` is set. See `docs/narrative.yaml`.

## Architecture

```mermaid
flowchart TB
    subgraph requests [Incoming Requests]
        R1[npm install]
        R2[npm install]
        R3[npm install]
    end

    subgraph proxy [npm-cache-proxy]
        subgraph memory [In-Memory Layer]
            L1[L1: Metadata Cache]
            ACL[Access Time Map]
        end
        L2[L2: SQLite Database]
        FS[Filesystem: Tarballs]
        FLUSH[Flush Goroutine]
    end

    R1 --> L1
    R2 --> L1
    R3 --> L1
    L1 -->|miss| L2
    L1 -->|record access| ACL
    ACL -->|batch flush every 30s| FLUSH
    FLUSH --> L2
    L2 -->|file path| FS
    FS -->|stream directly| R1

    subgraph upstream [Upstream]
        NPM[registry.npmjs.org]
    end

    L2 -->|miss| NPM
```

## Request flow

```mermaid
sequenceDiagram
    participant Client
    participant Proxy
    participant L1 as L1 Memory
    participant ACL as Access Map
    participant L2 as L2 SQLite
    participant FS as Filesystem

    Client->>Proxy: GET /lodash
    Proxy->>L1: Get("lodash")
    alt L1 Hit
        L1-->>Proxy: metadata
        Proxy->>ACL: Record access (async)
    else L1 Miss
        Proxy->>L2: Get("lodash")
        L2-->>Proxy: metadata
        Proxy->>L1: Store in L1
        Proxy->>ACL: Record access (async)
    end
    Proxy-->>Client: Return metadata JSON

    Note over Client,Proxy: Later: tarball request
    Client->>Proxy: GET /lodash/-/lodash-4.17.21.tgz
    Proxy->>L1: Get file path
    L1-->>Proxy: /cache/lodash-4.17.21.tgz
    Proxy->>FS: http.ServeFile (streaming)
    FS-->>Client: Stream tarball bytes directly

    Note over ACL,L2: Background (every 30s)
    ACL->>L2: Batch flush access times
```
