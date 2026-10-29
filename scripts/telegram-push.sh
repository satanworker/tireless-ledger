#!/usr/bin/env bash
# Export your Telegram chats on this Mac and push them to a Tireless server
# that indexes them (e.g. imp-home's telegram-inbox). The Telethon login stays
# here; the server only receives the JSONL export, through an rsync-only key.
#
#   telegram-push.sh setup     once: name, server, API id/hash, SSH key, login
#   telegram-push.sh chats     list your chats with their ids
#   telegram-push.sh run       export the allowlisted chats and push them
#   telegram-push.sh install   run every 15 minutes (launchd)
#
# Needs Nix (the exporter is the tireless-ledger flake's tireless-telegram)
# and a Tailscale connection to the server.
set -euo pipefail

CONF="$HOME/.config/telegram-push"
STATE="$HOME/.local/state/tireless-telegram"
KEY="$HOME/.ssh/telegram-push"
LABEL="dev.tireless.telegram-push"
FLAKE="github:satanworker/tireless-ledger"

load() {
  # shellcheck source=/dev/null
  . "$CONF/env"
  export TELEGRAM_API_ID TELEGRAM_API_HASH
  mkdir -p "$STATE"
}

setup() {
  mkdir -p "$CONF" "$STATE"
  chmod 700 "$CONF" "$STATE"
  if [ ! -s "$CONF/env" ]; then
    read -r -p 'your name (as the server knows it, e.g. devin): ' name
    read -r -p 'server [100.97.80.65]: ' server
    read -r -p 'Telegram api_id: ' api_id
    read -r -s -p 'Telegram api_hash: ' api_hash
    echo
    (umask 077 && printf 'PUSH_NAME=%q\nPUSH_SERVER=%q\nTELEGRAM_API_ID=%q\nTELEGRAM_API_HASH=%q\n' \
      "$name" "${server:-100.97.80.65}" "$api_id" "$api_hash" > "$CONF/env")
  fi
  [ -e "$CONF/chats" ] || printf '# One chat id per line (see: telegram-push.sh chats).\n' > "$CONF/chats"
  [ -e "$KEY" ] || ssh-keygen -q -t ed25519 -N '' -C "telegram-push@$(hostname -s)" -f "$KEY"
  echo "Send this public key to the server admin:"
  cat "$KEY.pub"
  echo "Logging in to Telegram (phone number, then the code from the app)."
  echo "Never forward that code through Telegram: Telegram then blocks the login."
  chats
  echo "Done. Add chat ids from the list above to $CONF/chats, then: $0 run && $0 install"
}

chats() {
  load
  # Telethon session shared with the exporter; the first use logs in, reading
  # the phone number and code from the terminal (so the script goes in -c).
  # shellcheck disable=SC2016 # Nix expression, not shell
  nix shell --impure --expr '(builtins.getFlake "nixpkgs").legacyPackages.${builtins.currentSystem}.python3.withPackages (ps: [ ps.telethon ])' \
    -c python3 -c '
import os, sys
from telethon.sync import TelegramClient
with TelegramClient(sys.argv[1], int(os.environ["TELEGRAM_API_ID"]), os.environ["TELEGRAM_API_HASH"]) as c:
    for d in c.iter_dialogs():
        kind = "dm" if d.is_user else "group" if d.is_group else "channel"
        print(f"{d.id}\t{kind}\t{d.name}")
' "$STATE/account.session"
}

run() {
  load
  if ! grep -qv '^[[:space:]]*\(#\|$\)' "$CONF/chats"; then
    echo "no chats in $CONF/chats: nothing to export" >&2
    exit 1
  fi
  nix run "$FLAKE#tireless-telegram" -- \
    --chats "$CONF/chats" --types dm,group,topic --since 2026-09-26
  rsync -a --exclude '*.tmp' \
    -e "ssh -i $KEY -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new" \
    "$STATE/export/" "tg-$PUSH_NAME@$PUSH_SERVER:"
}

install() {
  local plist="$HOME/Library/LaunchAgents/$LABEL.plist" self
  self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  mkdir -p "$(dirname "$plist")" "$HOME/Library/Logs"
  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key><array><string>$self</string><string>run</string></array>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>/nix/var/nix/profiles/default/bin:/usr/bin:/bin</string>
  </dict>
  <key>StartInterval</key><integer>900</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/telegram-push.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/telegram-push.log</string>
</dict></plist>
EOF
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  echo "installed $plist (log: ~/Library/Logs/telegram-push.log)"
}

case "${1:-}" in
  setup | chats | run | install) "$1" ;;
  *) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
