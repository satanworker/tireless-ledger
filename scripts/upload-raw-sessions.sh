#!/usr/bin/env bash
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

# Deliberately dumb client: copy original JSONL files to S3. Parsing,
# deduplication, embeddings, and indexing all happen on the server.
UPLOAD_ENV="${TIRELESS_UPLOAD_ENV:-$HOME/.config/tireless-ledger/raw-upload.env}"
if [[ ! -f "$UPLOAD_ENV" ]]; then
  echo "missing $UPLOAD_ENV" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "$UPLOAD_ENV"
set +a

: "${TIRELESS_RAW_URL:?set TIRELESS_RAW_URL, e.g. s3://bucket/session-recall-raw-v1}"
TIRELESS_UPLOAD_HOST="${TIRELESS_UPLOAD_HOST:-$(hostname -s)}"
TIRELESS_CODEX_SESSIONS="${TIRELESS_CODEX_SESSIONS:-$HOME/.codex/sessions}"
TIRELESS_PI_SESSIONS="${TIRELESS_PI_SESSIONS:-$HOME/.pi/agent/sessions}"

case "$TIRELESS_UPLOAD_HOST" in
  *[!A-Za-z0-9._-]*|'') echo "invalid TIRELESS_UPLOAD_HOST=$TIRELESS_UPLOAD_HOST" >&2; exit 1 ;;
esac

exec tireless-upload \
  --raw-url "$TIRELESS_RAW_URL" \
  --endpoint "${TIRELESS_S3_ENDPOINT:-}" \
  --host "$TIRELESS_UPLOAD_HOST" \
  --pi "$TIRELESS_PI_SESSIONS" \
  --codex "$TIRELESS_CODEX_SESSIONS"
