package proxy

import (
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GetCachedPath returns cached upstream response for a given url path.
func (proxy Proxy) GetCachedPath(options Options, path string, request *http.Request) ([]byte, error) {
	key := options.DatabasePrefix + path

	// get package from database
	pkg, err := proxy.Database.Get(key)

	// If package is found, refresh expiration (sliding expiration)
	if err == nil {
		// Refresh expiration to extend TTL on access
		if refreshErr := proxy.Database.RefreshExpiration(key, options.DatabaseExpiration); refreshErr != nil {
			// Log but don't fail - caching is best effort
			log.Printf("Warning: Failed to refresh expiration for %s: %v", path, refreshErr)
		}
		return []byte(pkg), nil
	}

	// either package doesn't exist or there's some other problem
	if err != nil {

		// check if error is caused by nonexistent package
		// Support both Redis ("redis: nil") and SQLite ("key not found") error formats
		errStr := err.Error()
		if errStr != "redis: nil" && errStr != "key not found" {
			return nil, err
		}

		// error is caused by nonexistent package
		// fetch package
		req, err := http.NewRequest("GET", options.UpstreamAddress+path, nil)

		req.Header = request.Header
		req.Header.Set("Accept-Encoding", "gzip")

		res, err := proxy.HttpClient.Do(req)
		if err != nil {
			return nil, err
		}

		if res.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(res.Body)
			if err != nil {
				log.Fatal(err)
			}

			res.Body = zr
		}

		defer res.Body.Close()
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}

		pkg = string(body)

		// TODO: avoid calling MustCompile every time
		// find "dist": "https?://.*/ and replace to "dist": "{localurl}/
		pkg = regexp.MustCompile(`(?U)"tarball":"https?://.*/`).ReplaceAllString(string(body), `"tarball": "http://`+request.Host+"/")

		// save to storage backend
		err = proxy.Database.Set(key, pkg, options.DatabaseExpiration)
		if err != nil {
			log.Printf("Error: Failed to cache package metadata for %s: %v", path, err)
			// Continue anyway - return the package even if caching fails
		}
	}

	return []byte(pkg), nil
}

// GetCachedTarball returns cached tarball file for a given url path.
func (proxy Proxy) GetCachedTarball(options Options, path string, request *http.Request) ([]byte, error) {
	if options.PackageDir == "" {
		// If no package directory configured, proxy directly without caching
		return proxy.fetchTarball(options, path, request)
	}

	key := options.DatabasePrefix + path
	
	// Check if tarball metadata exists in database
	tarballKey := key + ":tarball"
	filePath, err := proxy.Database.Get(tarballKey)
	
	if err == nil && filePath != "" {
		// Tarball is cached, refresh expiration (sliding expiration)
		if refreshErr := proxy.Database.RefreshExpiration(tarballKey, options.DatabaseExpiration); refreshErr != nil {
			// Log but don't fail - caching is best effort
			log.Printf("Warning: Failed to refresh tarball expiration for %s: %v", path, refreshErr)
		}
		
		// Read from filesystem
		data, err := ioutil.ReadFile(filePath)
		if err == nil {
			return data, nil
		}
		// If file read fails, fall through to re-download
	}

	// Tarball not cached or file missing, fetch and cache it
	// Use request deduplication to prevent concurrent downloads of the same tarball
	return proxy.fetchAndCacheTarballWithDedup(options, path, request, key, tarballKey)
}

// fetchAndCacheTarballWithDedup ensures only one goroutine downloads a given tarball at a time
func (proxy Proxy) fetchAndCacheTarballWithDedup(options Options, path string, request *http.Request, key, tarballKey string) ([]byte, error) {
	// Check if another goroutine is already downloading this tarball
	downloadChan, loaded := proxy.downloadLocks.LoadOrStore(tarballKey, make(chan struct{}))
	ch := downloadChan.(chan struct{})
	
	if loaded {
		// Another goroutine is downloading, wait for it to complete
		<-ch
		// After download completes, check cache again
		filePath, err := proxy.Database.Get(tarballKey)
		if err == nil && filePath != "" {
			// Refresh expiration
			if refreshErr := proxy.Database.RefreshExpiration(tarballKey, options.DatabaseExpiration); refreshErr != nil {
				log.Printf("Warning: Failed to refresh tarball expiration for %s: %v", path, refreshErr)
			}
			// Read from filesystem
			return ioutil.ReadFile(filePath)
		}
		// If still not cached, fall through to download (shouldn't happen normally)
		// This could happen if the download failed, so we'll try again
		return proxy.fetchAndCacheTarball(options, path, request, key, tarballKey)
	}
	
	// We're the first goroutine, do the download
	defer func() {
		// Signal other waiting goroutines that download is complete
		close(ch)
		proxy.downloadLocks.Delete(tarballKey)
	}()
	
	return proxy.fetchAndCacheTarball(options, path, request, key, tarballKey)
}

// fetchTarball downloads a tarball from upstream without caching.
func (proxy Proxy) fetchTarball(options Options, path string, request *http.Request) ([]byte, error) {
	req, err := http.NewRequest("GET", options.UpstreamAddress+path, nil)
	if err != nil {
		return nil, err
	}

	req.Header = request.Header
	req.Header.Set("Accept-Encoding", "gzip")

	res, err := proxy.HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var reader io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(res.Body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		reader = zr
	}

	body, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	return body, nil
}

// fetchTarballToFile downloads a tarball and streams it directly to a file.
// Returns the file path and error. This is more efficient for large files.
func (proxy Proxy) fetchTarballToFile(options Options, path string, request *http.Request, filePath string) (int64, error) {
	req, err := http.NewRequest("GET", options.UpstreamAddress+path, nil)
	if err != nil {
		return 0, err
	}

	req.Header = request.Header
	req.Header.Set("Accept-Encoding", "gzip")

	res, err := proxy.HttpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()

	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return 0, fmt.Errorf("failed to create directory: %w", err)
	}

	// Create file for writing
	file, err := os.Create(filePath)
	if err != nil {
		return 0, fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	var reader io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(res.Body)
		if err != nil {
			return 0, err
		}
		defer zr.Close()
		reader = zr
	}

	// Stream directly to file instead of loading into memory
	written, err := io.Copy(file, reader)
	if err != nil {
		os.Remove(filePath) // Clean up partial file on error
		return 0, err
	}

	return written, nil
}

// fetchAndCacheTarball downloads a tarball and caches it to the filesystem.
// Optimized to stream directly to disk instead of loading into memory.
func (proxy Proxy) fetchAndCacheTarball(options Options, path string, request *http.Request, key, tarballKey string) ([]byte, error) {
	// Check if PackageDir is configured
	if options.PackageDir == "" {
		log.Printf("Warning: PackageDir not configured, cannot cache tarball %s", path)
		// Fall back to in-memory download
		return proxy.fetchTarball(options, path, request)
	}

	// Generate safe filename from path
	// Remove leading slash and replace slashes with underscores
	safePath := strings.TrimPrefix(path, "/")
	safePath = strings.ReplaceAll(safePath, "/", "_")
	safePath = strings.ReplaceAll(safePath, "@", "_at_")
	
	// Ensure safe filename
	safePath = filepath.Clean(safePath)
	if strings.Contains(safePath, "..") {
		safePath = filepath.Base(safePath)
	}

	filePath := filepath.Join(options.PackageDir, safePath)

	// Stream download directly to file (more efficient for large files)
	written, err := proxy.fetchTarballToFile(options, path, request, filePath)
	if err != nil {
		log.Printf("Error: Failed to fetch tarball %s: %v", path, err)
		return nil, err
	}

	log.Printf("Cached tarball: %s -> %s (%d bytes)", path, filePath, written)

	// Store metadata in database
	// Store file path with expiration
	if err := proxy.Database.Set(tarballKey, filePath, options.DatabaseExpiration); err != nil {
		// If storing metadata fails, log but don't fail the request
		// The file is cached on disk even if metadata storage fails
		log.Printf("Warning: failed to store tarball metadata for %s: %v", path, err)
	}

	// Read file back into memory to return to client
	// TODO: Consider streaming response directly from file in the future for even better performance
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Printf("Error: Failed to read cached tarball %s: %v", filePath, err)
		return nil, err
	}

	return data, nil
}

// ListCachedPaths returns list of all cached url paths.
func (proxy Proxy) ListCachedPaths(options Options) ([]string, error) {
	metadata, err := proxy.Database.Keys(options.DatabasePrefix)
	if err != nil {
		return nil, err
	}

	deprefixedMetadata := make([]string, 0)
	for _, record := range metadata {
		deprefixedMetadata = append(deprefixedMetadata, strings.Replace(record, options.DatabasePrefix, "", 1))
	}

	return deprefixedMetadata, nil
}

// PurgeCachedPaths deletes all cached url paths.
func (proxy Proxy) PurgeCachedPaths(options Options) error {
	metadata, err := proxy.Database.Keys(options.DatabasePrefix)
	if err != nil {
		return err
	}

	for _, record := range metadata {
		err := proxy.Database.Delete(record)
		if err != nil {
			return err
		}
	}

	return nil
}
