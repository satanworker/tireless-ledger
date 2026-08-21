---
id: nix-mac-cli-ac-only-ingest-agent
title: Nix Mac CLI + AC-only ingest agent
emoji: 💎
status: completed
created: 2026-08-20T12:08:49.796Z
updated: 2026-08-20T12:51:24.903Z
---
## Checklist
- [x] Package tireless in NixOS/satanworker pkgs and homePackages
- [x] Home Manager launchd agent: AC-only, CPU llama, sync, kill llama
- [x] launchd: Background/Nice/LowPriorityIO, StartInterval 600, no KeepAlive
- [x] Local logs: NixOS/satanworker/tmp/tireless-ingest.log (gitignored tmp/)
    launchd stdout/stderr there, one skip line on battery; no log lib, not ~/Library/Logs

## Notes
Logs: ~/Developer/NixOS/satanworker/tmp/tireless-ingest.log (+ .err). That dir is already in .gitignore. Not ~/Library/Logs, not /tmp. Wrapper mkdir -p before launchd writes.
