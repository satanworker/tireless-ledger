"""Compare every parsed session-memory ID with a deployed pi-memoryd index."""

from __future__ import annotations

import argparse
import json
import socket
import sys
import urllib.parse
import urllib.request
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

from parse import walk_roots


def fetch_session(server: str, key: tuple[str, str, str]) -> list[dict]:
    session_id, host, harness = key
    query = urllib.parse.urlencode(
        {"session_id": session_id, "host": host, "harness": harness, "limit": 100_000}
    )
    with urllib.request.urlopen(
        server.rstrip("/") + "/v1/memory/session?" + query, timeout=180
    ) as response:
        return json.load(response).get("results") or []


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--server", required=True)
    parser.add_argument("--host", default=socket.gethostname())
    parser.add_argument("--pi", type=Path, default=Path.home() / ".pi/agent/sessions")
    parser.add_argument("--codex", type=Path, default=Path.home() / ".codex/sessions")
    parser.add_argument("--workers", type=int, default=4)
    args = parser.parse_args()

    expected: dict[tuple[str, str, str], dict[str, dict]] = defaultdict(dict)
    conflicts: list[str] = []
    for turn in walk_roots(args.pi, args.codex, args.host):
        metadata = turn["metadata"]
        key = (metadata["session_id"], metadata["host"], metadata["harness"])
        previous = expected[key].get(turn["id"])
        if previous is not None:
            if previous["metadata"]["file_hash"] != metadata["file_hash"]:
                conflicts.append(turn["id"])
            continue
        expected[key][turn["id"]] = turn

    errors: list[str] = []
    actual_rows = 0
    actual_unique = 0
    with ThreadPoolExecutor(max_workers=max(1, args.workers)) as pool:
        futures = {pool.submit(fetch_session, args.server, key): key for key in expected}
        for future in as_completed(futures):
            key = futures[future]
            want = expected[key]
            try:
                rows = future.result()
            except Exception as exc:  # report every failed session in one audit
                errors.append(f"{key}: request failed: {exc}")
                continue
            ids = [str(row.get("id") or "") for row in rows]
            unique_ids = set(ids)
            actual_rows += len(ids)
            actual_unique += len(unique_ids)
            if len(ids) != len(unique_ids):
                errors.append(f"{key}: duplicate rows={len(ids) - len(unique_ids)}")
            missing = set(want) - unique_ids
            extra = unique_ids - set(want)
            if missing:
                errors.append(f"{key}: missing IDs={len(missing)}")
            if extra:
                errors.append(f"{key}: extra IDs={len(extra)}")
            for row in rows:
                record_id = str(row.get("id") or "")
                source = want.get(record_id)
                if source is None:
                    continue
                got_meta = row.get("metadata") or {}
                want_meta = source["metadata"]
                for field in ("file_hash", "file_path", "session_id", "host", "harness", "role"):
                    if str(got_meta.get(field) or "") != str(want_meta[field]):
                        errors.append(f"{key}: {record_id} differs in {field}")
                        break

    expected_rows = sum(len(records) for records in expected.values())
    report = {
        "sessions": len(expected),
        "expected_unique_ids": expected_rows,
        "actual_rows": actual_rows,
        "actual_unique_ids": actual_unique,
        "source_id_conflicts": len(conflicts),
        "errors": errors[:50],
    }
    print(json.dumps(report, indent=2))
    if conflicts or errors or actual_rows != expected_rows or actual_unique != expected_rows:
        sys.exit(1)


if __name__ == "__main__":
    main()
