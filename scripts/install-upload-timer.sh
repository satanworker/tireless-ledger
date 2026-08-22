#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
unit_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"

GOCACHE="${GOCACHE:-/tmp/tireless-ledger-go-cache}" make -C "$project_dir" install-uploader-bin
install -d -m 0755 "$unit_dir"
install -m 0644 "$project_dir/systemd/tireless-ledger-upload.service" "$unit_dir/"
install -m 0644 "$project_dir/systemd/tireless-ledger-upload.timer" "$unit_dir/"

systemctl --user daemon-reload
systemctl --user enable --now tireless-ledger-upload.timer
systemctl --user start tireless-ledger-upload.service
systemctl --user list-timers tireless-ledger-upload.timer --no-pager
