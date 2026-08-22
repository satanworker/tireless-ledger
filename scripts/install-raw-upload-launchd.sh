#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LABEL="com.earendil.tireless-raw-upload"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/tireless-ledger"
mkdir -p "$(dirname "$PLIST")" "$LOG_DIR"

sed \
  -e "s|__UPLOAD_SCRIPT__|$ROOT/scripts/upload-raw-sessions.sh|g" \
  -e "s|__STDOUT__|$LOG_DIR/raw-upload.log|g" \
  -e "s|__STDERR__|$LOG_DIR/raw-upload-error.log|g" \
  "$ROOT/config/com.earendil.tireless-raw-upload.plist" > "$PLIST"

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
echo "installed $PLIST"
