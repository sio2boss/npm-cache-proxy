package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	ginzap "github.com/gin-contrib/zap"
	gin "github.com/gin-gonic/gin"
	zap "go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// ServerOptions provides configuration for Server method
type ServerOptions struct {
	ListenAddress string
	Silent        bool
	Dev           bool

	GetOptions func() (Options, error)
}

// Server creates http proxy server
func (proxy Proxy) Server(options ServerOptions) *http.Server {
	gin.SetMode("release")

	router := gin.New()

	if options.Silent {
		router.Use(gin.Recovery())
	} else {
		var logger *zap.Logger
		if options.Dev {
			// Use custom formatter matching Makefile format
			logger = newDevLogger()
		} else {
			// Use production JSON logger
			logger, _ = zap.NewProduction()
		}
		router.Use(ginzap.Ginzap(logger, time.RFC3339, true))
		router.Use(ginzap.RecoveryWithZap(logger, true))
	}

	// Add a catch-all middleware to log ALL requests for debugging
	if options.Dev {
		router.Use(func(c *gin.Context) {
			// Log the request before it's processed with full URL and query string
			fullURL := c.Request.URL.Path
			if c.Request.URL.RawQuery != "" {
				fullURL += "?" + c.Request.URL.RawQuery
			}
			// Pad method to 4 characters to match INFO log format alignment
			method := strings.ToUpper(c.Request.Method)
			methodPadded := fmt.Sprintf("%-4s", method)
			if len(methodPadded) > 4 {
				methodPadded = methodPadded[:4]
			}
			fmt.Printf("[DEBUG] %s %s (Host: %s)\n", methodPadded, fullURL, c.Request.Host)
			c.Next()
		})
	}
	
	router.GET("/:scope/:name", proxy.getPackageHandler(options))
	router.GET("/:scope", proxy.getPackageHandler(options))
	router.NoRoute(proxy.noRouteHandler(options))

	return &http.Server{
		Handler: router,
		Addr:    options.ListenAddress,
	}
}

func (proxy Proxy) getPackageHandler(options ServerOptions) gin.HandlerFunc {
	return func(c *gin.Context) {
		opts, err := options.GetOptions()

		if err != nil {
			c.AbortWithError(500, err)
			return
		}
		
		pkg, err := proxy.GetCachedPath(opts, c.Request.URL.Path, c.Request)

		if err != nil {
			// Log error but don't expose details to client
			if options.Dev {
				fmt.Printf("[ERROR] Failed to get cached path %s: %v\n", c.Request.URL.Path, err)
			}
			c.AbortWithError(500, err)
			return
		}
		
		c.Header("Cache-Control", "public, max-age="+strconv.Itoa(int(opts.DatabaseExpiration.Seconds())))
		c.Data(200, "application/json", pkg)
	}
}

func (proxy Proxy) noRouteHandler(options ServerOptions) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Handle all HTTP methods, not just GET
		if c.Request.Method != "GET" && c.Request.Method != "HEAD" {
			// For POST/PUT/DELETE/etc, proxy to upstream instead of redirecting
			// This prevents npm from following redirects and bypassing the proxy
			opts, err := options.GetOptions()
			if err != nil {
				c.AbortWithStatusJSON(500, err)
				return
			}
			proxy.proxyRequest(c, opts.UpstreamAddress+c.Request.URL.Path)
			return
		}
		
		if strings.Contains(c.Request.URL.Path, ".tgz") {
			// handle package file request
			opts, err := options.GetOptions()
			if err != nil {
				if options.Dev {
					fmt.Printf("[ERROR] Failed to get options for tarball %s: %v\n", c.Request.URL.Path, err)
				}
				c.AbortWithError(500, err)
				return
			}
			
			result, err := proxy.GetCachedTarball(opts, c.Request.URL.Path, c.Request)
			if err != nil {
				if options.Dev {
					fmt.Printf("[ERROR] Failed to get cached tarball %s: %v\n", c.Request.URL.Path, err)
				}
				c.AbortWithError(500, err)
				return
			}
			
			c.Header("Cache-Control", "public, max-age="+strconv.Itoa(int(opts.DatabaseExpiration.Seconds())))
			
			// Stream file directly if we have a file path (zero-copy, minimal memory)
			// Fall back to in-memory response if no file path available
			if result.FilePath != "" {
				// Use http.ServeFile for efficient streaming with sendfile() syscall
				c.Header("Content-Type", "application/octet-stream")
				http.ServeFile(c.Writer, c.Request, result.FilePath)
			} else {
				// Fallback: serve from memory (when PackageDir not configured)
				c.Data(200, "application/octet-stream", result.Data)
			}
		} else if c.Request.URL.Path == "/" {
			// get health
			err := proxy.Database.Health()

			if err != nil {
				c.AbortWithStatusJSON(503, err)
			} else {
				c.AbortWithStatusJSON(200, gin.H{"ok": true})
			}
		} else {
			// proxy GET requests that don't match package routes
			// This includes things like /-/v1/search, /-/all, etc.
			if options.Dev {
				fmt.Printf("[DEBUG] Proxying unmatched GET request: %s\n", c.Request.URL.Path)
			}
			opts, err := options.GetOptions()
			if err != nil {
				c.AbortWithStatusJSON(500, err)
				return
			}
			proxy.proxyRequest(c, opts.UpstreamAddress+c.Request.URL.Path+c.Request.URL.RawQuery)
		}
	}
}

// proxyRequest forwards a request to the upstream server and returns the response
func (proxy Proxy) proxyRequest(c *gin.Context, upstreamURL string) {
	// Build full URL with query string if present
	if c.Request.URL.RawQuery != "" && !strings.Contains(upstreamURL, "?") {
		upstreamURL += "?" + c.Request.URL.RawQuery
	}
	
	// Create request to upstream
	req, err := http.NewRequest(c.Request.Method, upstreamURL, c.Request.Body)
	if err != nil {
		c.AbortWithStatusJSON(500, gin.H{"error": "failed to create upstream request"})
		return
	}

	// Copy headers from original request
	for key, values := range c.Request.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Remove Host header to let Go set it automatically
	req.Header.Del("Host")

	// Forward request to upstream
	resp, err := proxy.HttpClient.Do(req)
	if err != nil {
		c.AbortWithStatusJSON(502, gin.H{"error": "failed to connect to upstream"})
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	// Set status code
	c.Status(resp.StatusCode)

	// Copy response body
	_, err = c.Writer.Write([]byte{})
	if err == nil {
		_, err = io.Copy(c.Writer, resp.Body)
	}
	if err != nil {
		// Error writing response, but we've already started writing
		if c.Writer.Status() == 200 {
			c.AbortWithStatus(500)
		}
	}
}

// newDevLogger creates a zap logger with custom formatter matching Makefile format
// Format: [INFO] GET /path -> 200 (310ms) or [ERROR] GET /path -> 500: error message
func newDevLogger() *zap.Logger {
	// Create a custom encoder that formats output like Makefile
	encoderConfig := zap.NewDevelopmentConfig()
	encoder := &makefileEncoder{
		Encoder: zapcore.NewJSONEncoder(encoderConfig.EncoderConfig),
	}
	
	// Create core with custom writer that formats output
	core := zapcore.NewCore(
		encoder,
		zapcore.AddSync(&makefileWriter{}),
		zapcore.InfoLevel,
	)
	
	return zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
}

// makefileEncoder wraps the encoder to format output like Makefile
type makefileEncoder struct {
	zapcore.Encoder
}

func (e *makefileEncoder) EncodeEntry(entry zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	// Extract fields we need from structured log
	var method, path, statusStr, msg string
	var status int
	var latency time.Duration
	
	for _, field := range fields {
		switch field.Key {
		case "method":
			method = field.String
		case "path":
			path = field.String
		case "status":
			if field.Type == zapcore.Int64Type {
				status = int(field.Integer)
			} else if field.Type == zapcore.StringType {
				statusStr = field.String
			}
		case "msg":
			msg = field.String
		case "latency":
			if field.Type == zapcore.DurationType {
				latency = time.Duration(field.Integer)
			}
		}
	}
	
	// Format: [LEVEL]  METHOD PATH -> STATUS (LATENCYms) or [LEVEL]  METHOD PATH (for debug)
	// For non-HTTP logs: [LEVEL]  -    message
	buf := buffer.NewPool().Get()
	level := strings.ToUpper(entry.Level.String())
	
	// Format level as [LEVEL] (no padding inside brackets)
	levelFormatted := fmt.Sprintf("[%s]", level)
	
	// Pad method to exactly 4 characters, use "-" if no method
	methodUpper := strings.ToUpper(method)
	if methodUpper == "" {
		methodUpper = "-"
	}
	methodPadded := fmt.Sprintf("%-4s", methodUpper)
	if len(methodPadded) > 4 {
		methodPadded = methodPadded[:4]
	}
	
	// Use status from field or entry message
	if status == 0 && statusStr != "" {
		fmt.Sscanf(statusStr, "%d", &status)
	}
	
	// Determine if this is an HTTP request log (has path and method)
	isHTTPRequest := path != "" && method != ""
	
	if entry.Level == zapcore.ErrorLevel {
		if isHTTPRequest {
			buf.AppendString(fmt.Sprintf("%s  %s %s -> %s: %s\n", levelFormatted, methodPadded, path, statusStr, msg))
		} else {
			// Error without HTTP context
			buf.AppendString(fmt.Sprintf("%s  %s %s\n", levelFormatted, methodPadded, msg))
		}
	} else if entry.Level == zapcore.WarnLevel {
		if isHTTPRequest {
			latencyMs := int(latency.Milliseconds())
			if latencyMs == 0 && latency > 0 {
				latencyMs = int(latency.Seconds() * 1000)
			}
			buf.AppendString(fmt.Sprintf("%s  %s %s -> %d (%dms)\n", levelFormatted, methodPadded, path, status, latencyMs))
		} else {
			// Warning without HTTP context
			buf.AppendString(fmt.Sprintf("%s  %s %s\n", levelFormatted, methodPadded, msg))
		}
	} else {
		// INFO, DEBUG, etc.
		if isHTTPRequest {
			// Check if we have a status code (response) or just a request (debug)
			if status > 0 || statusStr != "" {
				latencyMs := int(latency.Milliseconds())
				if latencyMs == 0 && latency > 0 {
					latencyMs = int(latency.Seconds() * 1000)
				}
				if status == 0 && statusStr != "" {
					buf.AppendString(fmt.Sprintf("%s  %s %s -> %s\n", levelFormatted, methodPadded, path, statusStr))
				} else {
					buf.AppendString(fmt.Sprintf("%s  %s %s -> %d (%dms)\n", levelFormatted, methodPadded, path, status, latencyMs))
				}
			} else {
				// Debug request without response yet
				buf.AppendString(fmt.Sprintf("%s  %s %s\n", levelFormatted, methodPadded, path))
			}
		} else {
			// Non-HTTP log message
			buf.AppendString(fmt.Sprintf("%s  %s %s\n", levelFormatted, methodPadded, msg))
		}
	}
	
	return buf, nil
}

// makefileWriter writes formatted output and truncates long lines
type makefileWriter struct{}

func (w *makefileWriter) Write(p []byte) (n int, err error) {
	line := string(p)
	// Remove trailing newline for processing
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	// Truncate long lines to 120 chars like Makefile
	if len(line) > 120 {
		line = line[:117] + "..."
	}
	fmt.Println(line)
	return len(p), nil
}
