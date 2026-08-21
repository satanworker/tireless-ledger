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

# R2 token lives in PI_MEMORYD_*; leftover AWS_* in SOPS is a 20-char AWS/B2 key.
if [[ -n "${PI_MEMORYD_KEY_ID:-}" ]]; then
  export AWS_ACCESS_KEY_ID="$PI_MEMORYD_KEY_ID"
fi
if [[ -n "${PI_MEMORYD_APPLICATION_KEY:-}" ]]; then
  export AWS_SECRET_ACCESS_KEY="$PI_MEMORYD_APPLICATION_KEY"
fi
export AWS_REGION="${AWS_REGION:-auto}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-$AWS_REGION}"
# New folder: old vector-index/ is String schema and cannot change.
export PI_MEMORYD_S3_PREFIX=session-recall
export PI_MEMORYD_DIMENSIONS="${PI_MEMORYD_DIMENSIONS:-384}"
export PI_MEMORYD_DISTANCE="${PI_MEMORYD_DISTANCE:-L2}"
export PI_MEMORYD_FLUSH_SECONDS="${PI_MEMORYD_FLUSH_SECONDS:-300}"
# VPS is small; SOPS laptop cache sizes OOM a 8G box.
export VECTOR_CACHE_MEMORY_BYTES="${VECTOR_CACHE_MEMORY_BYTES:-268435456}"
export VECTOR_CACHE_DISK_BYTES="${VECTOR_CACHE_DISK_BYTES:-2147483648}"
export VECTOR_SPLIT_THRESHOLD_VECTORS="${VECTOR_SPLIT_THRESHOLD_VECTORS:-2000}"
export VECTOR_MERGE_THRESHOLD_VECTORS="${VECTOR_MERGE_THRESHOLD_VECTORS:-500}"
export VECTOR_SPLIT_SEARCH_NEIGHBOURHOOD="${VECTOR_SPLIT_SEARCH_NEIGHBOURHOOD:-16}"
# Dashboard S3 URL is often https://host/bucket; path-style then doubles the bucket.
if [[ -n "${PI_MEMORYD_S3_ENDPOINT:-}" && -n "${PI_MEMORYD_S3_BUCKET:-}" ]]; then
  case "${PI_MEMORYD_S3_ENDPOINT}" in
    */"${PI_MEMORYD_S3_BUCKET}"|*/"${PI_MEMORYD_S3_BUCKET}/")
      export PI_MEMORYD_S3_ENDPOINT="${PI_MEMORYD_S3_ENDPOINT%/"${PI_MEMORYD_S3_BUCKET}"}"
      export PI_MEMORYD_S3_ENDPOINT="${PI_MEMORYD_S3_ENDPOINT%/}"
      ;;
  esac
fi
export AWS_ENDPOINT_URL="${AWS_ENDPOINT_URL:-${PI_MEMORYD_S3_ENDPOINT:-}}"
export AWS_ENDPOINT="${AWS_ENDPOINT:-${PI_MEMORYD_S3_ENDPOINT:-}}"

if [[ -z "${PI_MEMORYD_S3_BUCKET:-}" ]]; then
  echo "PI_MEMORYD_S3_BUCKET missing" >&2
  exit 1
fi
export PI_MEMORYD_S3_BUCKET

mkdir -p "$(dirname "$OUT")"
python3 - "$TEMPLATE" "$OUT" "$ENV_FILE" <<'PY'
import os, string, sys
src, dst, env_path = sys.argv[1], sys.argv[2], sys.argv[3]
# Keep VPS cache small even if SOPS has laptop sizes.
os.environ["VECTOR_CACHE_MEMORY_BYTES"] = "268435456"
os.environ["VECTOR_CACHE_DISK_BYTES"] = "2147483648"
os.environ["PI_MEMORYD_S3_PREFIX"] = os.environ.get("PI_MEMORYD_S3_PREFIX") or "session-recall"
with open(src, 'r', encoding='utf-8') as f:
    data = string.Template(f.read()).safe_substitute(os.environ)
with open(dst, 'w', encoding='utf-8') as f:
    f.write(data)
# Rewrite mapped keys into .env so compose env_file matches yaml.
keys = [
    "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION", "AWS_DEFAULT_REGION",
    "AWS_ENDPOINT_URL", "AWS_ENDPOINT",
    "PI_MEMORYD_S3_ENDPOINT", "PI_MEMORYD_S3_BUCKET", "PI_MEMORYD_S3_PREFIX",
    "VECTOR_CACHE_MEMORY_BYTES", "VECTOR_CACHE_DISK_BYTES",
]
kv = {}
with open(env_path, encoding="utf-8") as f:
    for line in f:
        line = line.rstrip("\n")
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        kv[k] = v
for k in keys:
    if os.environ.get(k):
        kv[k] = os.environ[k]
with open(env_path, "w", encoding="utf-8") as f:
    for k in sorted(kv):
        f.write(f"{k}={kv[k]}\n")
PY
chmod 0600 "$OUT" "$ENV_FILE"
echo "wrote $OUT"
