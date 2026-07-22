#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${1:-$ROOT/.env}"
TEMPLATE="$ROOT/config/vector.s3.yaml.tmpl"
OUT="$ROOT/config/vector.s3.yaml"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "missing env file: $ENV_FILE" >&2
  echo "run: make secrets-decrypt" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

export AWS_REGION="${AWS_REGION:-eu-west-1}"
export PI_MEMORYD_S3_PREFIX="${PI_MEMORYD_S3_PREFIX:-vector-index}"
export PI_MEMORYD_DIMENSIONS="${PI_MEMORYD_DIMENSIONS:-384}"
export PI_MEMORYD_DISTANCE="${PI_MEMORYD_DISTANCE:-L2}"
export PI_MEMORYD_FLUSH_SECONDS="${PI_MEMORYD_FLUSH_SECONDS:-300}"
export VECTOR_CACHE_MEMORY_BYTES="${VECTOR_CACHE_MEMORY_BYTES:-1073741824}"
export VECTOR_CACHE_DISK_BYTES="${VECTOR_CACHE_DISK_BYTES:-96636764160}"
export VECTOR_SPLIT_THRESHOLD_VECTORS="${VECTOR_SPLIT_THRESHOLD_VECTORS:-2000}"
export VECTOR_MERGE_THRESHOLD_VECTORS="${VECTOR_MERGE_THRESHOLD_VECTORS:-500}"
export VECTOR_SPLIT_SEARCH_NEIGHBOURHOOD="${VECTOR_SPLIT_SEARCH_NEIGHBOURHOOD:-16}"

if [[ -z "${PI_MEMORYD_S3_BUCKET:-}" ]]; then
  echo "PI_MEMORYD_S3_BUCKET missing" >&2
  exit 1
fi
export PI_MEMORYD_S3_BUCKET

mkdir -p "$(dirname "$OUT")"
python3 - "$TEMPLATE" "$OUT" <<'PY'
import os, string, sys
src, dst = sys.argv[1], sys.argv[2]
with open(src, 'r', encoding='utf-8') as f:
    data = string.Template(f.read()).safe_substitute(os.environ)
with open(dst, 'w', encoding='utf-8') as f:
    f.write(data)
PY
chmod 0600 "$OUT"
echo "wrote $OUT"
