"""Extract user/assistant turns from pi + Codex session JSONL."""

from __future__ import annotations

import hashlib
import json
from datetime import datetime
from pathlib import Path
from typing import Any, Iterator

MIN_CHARS = 8
SKIP_USER_PREFIXES = ("# AGENTS.md", "<permissions instructions>", "<INSTRUCTIONS>")


def sha256_hex(s: str) -> str:
    return hashlib.sha256(s.encode()).hexdigest()


def record_id(host: str, source: str, container_id: str, native_id: str) -> str:
    return f"{host}/{source}/{container_id}/{native_id}"


def parse_ts(raw: Any) -> int:
    if raw is None:
        return 0
    if isinstance(raw, (int, float)):
        n = int(raw)
        return n // 1000 if n > 10_000_000_000 else n
    s = str(raw).strip()
    if not s:
        return 0
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        return int(datetime.fromisoformat(s).timestamp())
    except ValueError:
        return 0


def project_from_cwd(cwd: str) -> str:
    if not cwd:
        return "unknown"
    name = Path(cwd.rstrip("/")).name
    return name or "unknown"


def _parts_text(content: Any) -> str:
    if isinstance(content, str):
        return content.strip()
    if not isinstance(content, list):
        return ""
    bits: list[str] = []
    for part in content:
        if not isinstance(part, dict):
            continue
        t = part.get("type")
        if t in ("thinking", "reasoning"):
            continue
        text = part.get("text")
        if isinstance(text, str) and text.strip():
            bits.append(text.strip())
    return "\n".join(bits).strip()


def _skip_user(text: str) -> bool:
    head = text.lstrip()
    return any(head.startswith(p) for p in SKIP_USER_PREFIXES)


def iter_pi_turns(path: Path, host: str) -> Iterator[dict[str, Any]]:
    container_id = path.stem.split("_")[-1]
    project = "unknown"
    cwd = ""
    with path.open(errors="ignore") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                continue
            if obj.get("type") == "session":
                container_id = str(obj.get("id") or container_id)
                cwd = str(obj.get("cwd") or "")
                project = project_from_cwd(cwd)
                continue
            if obj.get("type") != "message":
                continue
            msg = obj.get("message") or {}
            role = msg.get("role")
            if role not in ("user", "assistant"):
                continue
            text = _parts_text(msg.get("content"))
            if len(text) < MIN_CHARS or (role == "user" and _skip_user(text)):
                continue
            turn_id = str(obj.get("id") or "")
            if not turn_id:
                continue
            yield _turn(
                host=host,
                source="pi",
                container_id=container_id,
                turn_id=turn_id,
                role=role,
                text=text,
                ts=parse_ts(obj.get("timestamp") or msg.get("timestamp")),
                project=project,
            )


def iter_codex_turns(path: Path, host: str) -> Iterator[dict[str, Any]]:
    parts = path.stem.rsplit("-", 5)
    container_id = "-".join(parts[-5:]) if len(parts) >= 5 else path.stem
    project = "unknown"
    seen: set[str] = set()
    with path.open(errors="ignore") as f:
        for i, line in enumerate(f):
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                continue
            pl = obj.get("payload") if isinstance(obj.get("payload"), dict) else {}
            if obj.get("type") == "session_meta":
                container_id = str(pl.get("id") or container_id)
                project = project_from_cwd(str(pl.get("cwd") or ""))
                continue
            if obj.get("type") != "response_item" or pl.get("type") != "message":
                continue
            role = pl.get("role")
            if role not in ("user", "assistant"):
                continue
            text = _parts_text(pl.get("content"))
            if len(text) < MIN_CHARS or (role == "user" and _skip_user(text)):
                continue
            # Codex repeats the same response_item when a rollout resumes or
            # forks. Its payload id remains stable across those copies, while
            # the JSONL line number does not. Prefer the source-owned id so a
            # replay merge-inserts the logical message instead of creating a
            # second row. Legacy rows without an id retain the ordinal fallback.
            source_id = str(pl.get("id") or "").strip()
            turn_id = source_id or f"line-{i}"
            key = source_id or f"legacy-{sha256_hex(text)[:16]}"
            if key in seen:
                continue
            seen.add(key)
            yield _turn(
                host=host,
                source="codex",
                container_id=container_id,
                turn_id=turn_id,
                role=role,
                text=text,
                ts=parse_ts(obj.get("timestamp")),
                project=project,
            )


def _turn(
    *,
    host: str,
    source: str,
    container_id: str,
    turn_id: str,
    role: str,
    text: str,
    ts: int,
    project: str,
) -> dict[str, Any]:
    return {
        "id": record_id(host, source, container_id, turn_id),
        "text": text,
        "metadata": {
            "source": source,
            "host": host,
            "container_id": container_id,
            "timestamp": ts or 1,
            "project": project,
            "author_id": role,
            "kind": "message",
        },
    }


def iter_turns(path: Path, host: str, source: str | None = None) -> Iterator[dict[str, Any]]:
    name = path.name
    if source == "pi" or (source is None and not name.startswith("rollout-")):
        yield from iter_pi_turns(path, host)
    if source == "codex" or (source is None and name.startswith("rollout-")):
        yield from iter_codex_turns(path, host)


def walk_roots(pi_root: Path | None, codex_root: Path | None, host: str) -> Iterator[dict[str, Any]]:
    if pi_root and pi_root.is_dir():
        for p in sorted(pi_root.rglob("*.jsonl")):
            yield from iter_pi_turns(p, host)
    if codex_root and codex_root.is_dir():
        for p in sorted(codex_root.rglob("*.jsonl")):
            yield from iter_codex_turns(p, host)
