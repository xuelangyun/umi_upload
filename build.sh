#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o umi_upload.exe .
ls -lh umi_upload.exe
