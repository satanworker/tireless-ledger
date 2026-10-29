# Push your Telegram chats to a team Tireless server

`scripts/telegram-push.sh` exports the Telegram chats you choose on your Mac
and pushes them to a Tireless server (for Valantis: `imp-home`), which indexes
them for recall.

What leaves your Mac: only the messages of the chats you list, as JSONL
(sender, date, text, reply/forward info). What stays on your Mac: your
Telegram login (`~/.local/state/tireless-telegram/account.session`) and the
Telegram API id/hash. The SSH key the script creates can only write into your
inbox on the server: no shell, no reading.

## You need

- **Nix.** If you don't have it:
  `curl -fsSL https://install.determinate.systems/nix | sh -s -- install`
- **Tailscale, with the server shared to you.** Accept the share invite from
  the server's admin; the server then shows up in your Tailscale machine list
  (imp-home: `100.97.80.65`). Check with `nc -z 100.97.80.65 22`.
- **A Telegram API id and hash.** Log in at <https://my.telegram.org>, open
  "API development tools", create an app (any name), copy `api_id` and
  `api_hash`. They identify the client app, not your account.

## Set up

```bash
mkdir -p ~/.local/bin && cd ~/.local/bin   # stays here: launchd runs it from this path
curl -fsSLO https://raw.githubusercontent.com/satanworker/tireless-ledger/main/scripts/telegram-push.sh
chmod +x telegram-push.sh
./telegram-push.sh setup
```

The commands below run from `~/.local/bin` too (or add it to your `PATH`).
`setup` asks for:

- **your name**: the server admin tells you (e.g. `devin`); it is your inbox
  account `tg-<name>` and how your messages are labelled;
- **server**: press Enter for imp-home;
- **api_id / api_hash** from above.

It then prints a public key (send it to the server admin) and logs in to
Telegram: your phone number, then the code Telegram sends to the app, and your
2FA password if you have one. Type the code yourself; never forward it through
Telegram, or Telegram blocks the login. Finally it lists your chats as
`<id> <kind> <name>`.

## Choose the chats

Put the ids of the chats to share, one per line, in
`~/.config/telegram-push/chats` (`#` starts a comment):

```text
# Valantis core
-1001234567890
# DM with Alice
123456789
```

Only these are exported, from 2026-09-26 on. Run `telegram-push.sh chats` to
list them again. Removing a chat stops new messages; already pushed messages
stay indexed.

## Start

Once the admin confirms your key is installed:

```bash
./telegram-push.sh run       # first export and push; prints messages per chat
./telegram-push.sh install   # then every 15 minutes in the background
```

Your messages are searchable on the server a few minutes after each push.
Log: `~/Library/Logs/telegram-push.log`.

## Stop

```bash
launchctl bootout gui/$(id -u)/dev.tireless.telegram-push
rm ~/Library/LaunchAgents/dev.tireless.telegram-push.plist
```

To log the exporter out of Telegram, end its session in Telegram (Settings →
Devices) and delete `~/.local/state/tireless-telegram/account.session`.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `Permission denied (publickey)` | Your key isn't on the server yet, or the name in `~/.config/telegram-push/env` differs from the one the admin used. |
| `Connection refused` / timeout | Tailscale is off, or the server isn't shared with you. |
| `no chats in …/chats` | The allowlist is empty. |
| `ApiIdInvalidError` | Wrong api_id/api_hash: fix them in `~/.config/telegram-push/env`. |
| `flood wait Ns` | Telegram rate limit; the exporter waits and continues. |
