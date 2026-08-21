#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
runtime_dir="${XDG_RUNTIME_DIR:-/tmp}"
lock_file="${runtime_dir}/tireless-ledger-optimize.lock"

exec 9>"${lock_file}"
if ! flock -n 9; then
  echo "tireless-ledger optimization is already running; skipping"
  exit 0
fi

cd "${project_dir}"
exec docker compose run --rm --no-deps --no-TTY pi-memoryd --optimize
