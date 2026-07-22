#!/usr/bin/env bash
# Run pi-memoryd as a native host process (no Docker).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BIN="${PI_MEMORYD_BIN:-$ROOT/bin/pi-memoryd}"
ENV_FILE="${ENV_FILE:-$ROOT/.env}"
SOPS_FILE="${SOPS_FILE:-$ROOT/secrets/pi-memoryd.sops.env}"
DATA_DIR="${PI_MEMORYD_DATA_DIR:-$HOME/.local/share/pi-memoryd}"

if [[ ! -x "$BIN" ]]; then
  echo "building native binary..." >&2
  make -C "$ROOT" build
fi

if [[ ! -f "$ENV_FILE" ]]; then
  echo "decrypting secrets -> $ENV_FILE" >&2
  sops -d "$SOPS_FILE" > "$ENV_FILE"
  chmod 0600 "$ENV_FILE"
fi

mkdir -p "$DATA_DIR"

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

: "${PI_MEMORYD_S3_BUCKET:?PI_MEMORYD_S3_BUCKET required in env/sops}"

export AWS_REGION="${AWS_REGION:-eu-west-1}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-$AWS_REGION}"
export AWS_ENDPOINT_URL="${AWS_ENDPOINT_URL:-${PI_MEMORYD_S3_ENDPOINT:-}}"
export AWS_ENDPOINT="${AWS_ENDPOINT:-${PI_MEMORYD_S3_ENDPOINT:-}}"
export PI_MEMORYD_STORAGE_URL="${PI_MEMORYD_STORAGE_URL:-s3://${PI_MEMORYD_S3_BUCKET}/${PI_MEMORYD_S3_PREFIX:-vector-index}}"
export PI_MEMORYD_STATE="${PI_MEMORYD_STATE:-$DATA_DIR/dedup_state.json}"
export PI_MEMORYD_VECTOR_CONFIG="${PI_MEMORYD_VECTOR_CONFIG:-$DATA_DIR/vector.yaml}"

args=(
  --listen "${PI_MEMORYD_LISTEN:-:8090}"
  --vector-url "${PI_MEMORYD_VECTOR_URL:-http://127.0.0.1:8080}"
  --storage-url "$PI_MEMORYD_STORAGE_URL"
  --aws-region "$AWS_REGION"
  --s3-endpoint "${PI_MEMORYD_S3_ENDPOINT:-}"
  --dimensions "${PI_MEMORYD_DIMENSIONS:-384}"
  --distance "${PI_MEMORYD_DISTANCE:-L2}"
  --flush-seconds "${PI_MEMORYD_FLUSH_SECONDS:-300}"
  --batch-threshold "${PI_MEMORYD_BATCH_THRESHOLD:-500}"
  --state "$PI_MEMORYD_STATE"
  --vector-config "$PI_MEMORYD_VECTOR_CONFIG"
)

case "${PI_MEMORYD_START_VECTOR:-false}" in
  1|true|yes|on) args+=(--start-vector) ;;
esac
case "${PI_MEMORYD_DRY_RUN_S3:-false}" in
  1|true|yes|on) args+=(--dry-run-s3) ;;
esac

exec "$BIN" "${args[@]}"
