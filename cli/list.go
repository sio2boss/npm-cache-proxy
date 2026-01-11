package cli

import (
	"fmt"
	"os"

	npmproxy "github.com/pkgems/npm-cache-proxy/proxy"
)

func runList(opts Options) {
	proxy, storageBackend, err := getProxy(opts)
	if err != nil {
		errStr := err.Error()
		if isDatabaseNotInitializedError(err) {
			fmt.Fprintf(os.Stderr, "Error: Database has not been initialized yet.\n")
			fmt.Fprintf(os.Stderr, "Please run 'ncp' first to start the server and initialize the database.\n")
			if opts.StorageBackend == "sqlite" {
				fmt.Fprintf(os.Stderr, "Example: ncp --sqlite-db-path %s\n", opts.SQLiteDBPath)
			} else {
				fmt.Fprintf(os.Stderr, "Example: ncp --redis-address %s\n", opts.RedisAddress)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Error: %s\n", errStr)
		}
		os.Exit(1)
	}

	// Ensure storage is closed when we're done
	defer func() {
		if storageBackend != nil {
			storageBackend.Close()
		}
	}()

	metadatas, err := proxy.ListCachedPaths(npmproxy.Options{
		DatabasePrefix: opts.RedisPrefix,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	for _, metadata := range metadatas {
		fmt.Println(metadata)
	}
}
