#!/bin/bash
# Test script to verify npm cache proxy is working

set -e

echo "=== Testing npm cache proxy ==="
echo ""

# Check if proxy is running
if ! curl -s http://localhost:8080/ > /dev/null; then
    echo "ERROR: Proxy is not running on localhost:8080"
    echo "Please start it with: cd .. && ./ncp --dev"
    exit 1
fi

echo "✓ Proxy is running"
echo ""

# Clean up from previous test
echo "Cleaning up previous test..."
rm -rf node_modules package-lock.json
echo ""

# First install - should fetch from upstream and cache
echo "=== First install (should cache packages) ==="
npm install
echo ""

# Check if packages were installed
if [ ! -d "node_modules" ]; then
    echo "ERROR: node_modules directory was not created"
    exit 1
fi

echo "✓ Packages installed successfully"
echo ""

# Check if packages are in cache database
echo "=== Checking cache database ==="
cd ..
if [ -f "data/cache.db" ]; then
    echo "✓ Cache database exists"
    # Use sqlite3 to check if there are cached packages
    if command -v sqlite3 &> /dev/null; then
        COUNT=$(sqlite3 data/cache.db "SELECT COUNT(*) FROM registry_cache;" 2>/dev/null || echo "0")
        echo "  Cached package metadata entries: $COUNT"
    fi
else
    echo "⚠ Cache database not found (may be using Redis)"
fi
echo ""

# Check if package files are on disk
echo "=== Checking package files on disk ==="
if [ -d "data/packages" ]; then
    PACKAGE_COUNT=$(find data/packages -name "*.tgz" 2>/dev/null | wc -l | tr -d ' ')
    echo "✓ Package directory exists"
    echo "  Cached .tgz files: $PACKAGE_COUNT"
    if [ "$PACKAGE_COUNT" -gt 0 ]; then
        echo "  Sample files:"
        find data/packages -name "*.tgz" | head -5 | sed 's/^/    /'
    fi
else
    echo "⚠ Package directory not found"
fi
echo ""

# Second install - should use cache
echo "=== Second install (should use cache) ==="
cd example
rm -rf node_modules package-lock.json && npm cache clean --force
time npm install
echo ""

echo "=== Test complete ==="
echo ""
echo "If you see cached packages in the database and package files on disk,"
echo "the cache proxy is working correctly!"
