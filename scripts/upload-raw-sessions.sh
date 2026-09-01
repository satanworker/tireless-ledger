#!/usr/bin/env bash
set -euo pipefail
export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"

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

TIRELESS_RAW_URL="${TIRELESS_RAW_URL:-}"
if [[ -z "$TIRELESS_RAW_URL" && -n "${PI_MEMORYD_S3_BUCKET:-}" ]]; then
  TIRELESS_RAW_URL="s3://${PI_MEMORYD_S3_BUCKET}/${PI_MEMORYD_RAW_PREFIX:-session-recall-raw-v1}"
fi
TIRELESS_S3_ENDPOINT="${TIRELESS_S3_ENDPOINT:-${PI_MEMORYD_S3_ENDPOINT:-}}"
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-${PI_MEMORYD_KEY_ID:-}}"
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-${PI_MEMORYD_APPLICATION_KEY:-}}"
AWS_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-auto}}"
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_REGION
: "${TIRELESS_RAW_URL:?set TIRELESS_RAW_URL or PI_MEMORYD_S3_BUCKET}"
TIRELESS_UPLOAD_HOST="${TIRELESS_UPLOAD_HOST:-$(hostname -s)}"
TIRELESS_CODEX_SESSIONS="${TIRELESS_CODEX_SESSIONS:-$HOME/.codex/sessions}"
TIRELESS_PI_SESSIONS="${TIRELESS_PI_SESSIONS:-$HOME/.pi/agent/sessions}"
TIRELESS_OMP_SESSIONS="${TIRELESS_OMP_SESSIONS:-$HOME/.omp/agent/sessions}"

case "$TIRELESS_UPLOAD_HOST" in
  *[!A-Za-z0-9._-]*|'') echo "invalid TIRELESS_UPLOAD_HOST=$TIRELESS_UPLOAD_HOST" >&2; exit 1 ;;
esac

exec tireless-upload \
  --raw-url "$TIRELESS_RAW_URL" \
  --endpoint "${TIRELESS_S3_ENDPOINT:-}" \
  --host "$TIRELESS_UPLOAD_HOST" \
  --pi "$TIRELESS_PI_SESSIONS" \
  --codex "$TIRELESS_CODEX_SESSIONS" \
  --omp "$TIRELESS_OMP_SESSIONS"
