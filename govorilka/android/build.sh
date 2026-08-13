#!/usr/bin/env bash
# build.sh — собрать Android APK в Docker (SDK не нужен на хосте).
#
# Usage: ./build.sh
# Output: app/build/outputs/apk/debug/app-debug.apk
set -euo pipefail
cd "$(dirname "$0")"

echo "==> Build Docker image (first run downloads SDK)..."
docker build -t govorilka-android .

echo "==> Build APK..."
docker run --rm -v "$PWD":/app govorilka-android

APK="app/build/outputs/apk/debug/app-debug.apk"
echo "==> APK: $APK"
ls -la "$APK"
