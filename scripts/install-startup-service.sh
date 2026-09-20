#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
unit_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"

install -d -m 0755 "${unit_dir}"
install -m 0644 "${project_dir}/systemd/tireless-ledger-startup.service" "${unit_dir}/"

systemctl --user daemon-reload
systemctl --user enable --now tireless-ledger-startup.service
systemctl --user status tireless-ledger-startup.service --no-pager
