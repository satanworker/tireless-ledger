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

service="pi-memoryd"
container_id="$(docker compose ps -q "${service}")"
was_running=false
if [[ -n "${container_id}" ]] && [[ "$(docker inspect --format '{{.State.Running}}' "${container_id}")" == "true" ]]; then
  was_running=true
  docker compose stop "${service}"
fi

restart_service() {
  if [[ "${was_running}" == "true" ]]; then
    docker compose up -d --no-deps "${service}"
  fi
}
trap restart_service EXIT

# Lance maintenance compacts and prunes old table versions. It must not run
# beside a daemon that still has the previous manifest cached, otherwise that
# daemon can keep querying data/index files that pruning has just removed.
docker compose run --rm --no-deps --no-TTY "${service}" --optimize

restart_service
trap - EXIT
