package cli

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	npmproxy "github.com/pkgems/npm-cache-proxy/proxy"
	"github.com/pkgems/npm-cache-proxy/internal/storage"
)

func runServer(opts Options) {
	proxy, storageBackend, err := getProxy(opts)
	if err != nil {
		log.Fatal(err)
	}

	// Ensure storage is properly typed (needed for import recognition)
	var _ storage.Storage = storageBackend

	// Create HTTP server
	server := proxy.Server(npmproxy.ServerOptions{
		ListenAddress: opts.ListenAddress,
		Silent:        opts.Silent,
		Dev:           opts.Dev,

		GetOptions: func() (npmproxy.Options, error) {
			return npmproxy.Options{
				DatabasePrefix:     opts.RedisPrefix,
				DatabaseExpiration: time.Duration(opts.CacheTTL) * time.Second,
				UpstreamAddress:    opts.UpstreamAddress,
				PackageDir:         opts.PackageDir,
			}, nil
		},
	})

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start server in a goroutine
	serverErr := make(chan error, 1)
	go func() {
		log.Print("Listening on " + opts.ListenAddress)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// Wait for interrupt signal or server error
	select {
	case sig := <-sigChan:
		log.Printf("Received signal: %v. Shutting down gracefully...", sig)
	case err := <-serverErr:
		log.Printf("Server error: %v. Shutting down...", err)
	}

	// Gracefully shut down the HTTP server
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Error shutting down HTTP server: %v", err)
	} else {
		log.Print("HTTP server shut down successfully")
	}

	// Close storage backend (important for SQLite to unlock database and perform final checkpoint)
	if storageBackend != nil {
		if err := storageBackend.Close(); err != nil {
			log.Printf("Error closing storage backend: %v", err)
		} else {
			log.Print("Storage backend closed successfully")
		}
	}
}
