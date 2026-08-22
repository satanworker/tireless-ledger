"""Mac indexer: parse sessions, embed with bge-small, POST to pi-memoryd."""

from __future__ import annotations

import argparse
import json
import os
import socket
import sys
import urllib.error
import urllib.request
from pathlib import Path

from parse import walk_roots

QUERY_PREFIX = "Represent this sentence for searching relevant passages: "
DIMS = 384
POST_BATCH = 100
EMBED_MAX_CHARS = 800
DEFAULT_EMBED = "http://127.0.0.1:8091"


def _post(url: str, payload: dict) -> dict:
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        raise SystemExit(f"HTTP {e.code} {url}: {body}") from e


def _ingest_item(turn: dict, vector: list[float]) -> dict:
    meta = {k: v for k, v in turn["metadata"].items() if k != "source"}
    return {
        "id": turn["id"],
        "vector": vector,
        "forward_content": turn["forward_content"],
        "metadata": meta,
    }


def _load_known_files(path: Path | None) -> dict[str, str]:
    if path is None:
        return {}
    with path.open() as f:
        state = json.load(f)
    files = state.get("files") if isinstance(state, dict) else None
    if not isinstance(files, dict):
        raise SystemExit(f"invalid dedup state: {path}")
    return {str(k): str(v) for k, v in files.items()}


def cmd_parse(args: argparse.Namespace) -> None:
    n = 0
    for turn in walk_roots(args.pi, args.codex, args.host):
        n += 1
        if args.limit and n > args.limit:
            break
        if args.dump:
            print(json.dumps({"id": turn["id"], "meta": turn["metadata"], "text": turn["forward_content"][:200]}))
    print(f"turns={n if not args.limit else min(n, args.limit)}", file=sys.stderr)


def _embed_texts(embed_url: str, texts: list[str]) -> list[list[float]]:
    out = _post(embed_url.rstrip("/") + "/v1/embeddings", {"input": texts, "model": "bge-small-en-v1.5"})
    rows = sorted(out.get("data") or [], key=lambda r: int(r.get("index") or 0))
    vecs = [list(map(float, r["embedding"])) for r in rows]
    if len(vecs) != len(texts):
        raise SystemExit(f"embed count {len(vecs)} != {len(texts)}")
    if vecs and len(vecs[0]) != DIMS:
        raise SystemExit(f"model dims={len(vecs[0])} want={DIMS}")
    return vecs


def cmd_ingest(args: argparse.Namespace) -> None:
    accepted = skipped = n = 0
    url = args.server.rstrip("/") + "/v1/memory/ingest"
    buf: list[dict] = []
    known_files = _load_known_files(args.known_state)
    seen_ids: set[str] = set()

    def flush() -> None:
        nonlocal accepted, skipped, buf
        if not buf:
            return
        if args.dry_run:
            buf = []
            return
        # BGE-small has a 512-token input window. Keep the full text in Lance,
        # but bound the text sent to llama.cpp so a long Codex turn cannot
        # reject the entire ingest batch.
        texts = [t["forward_content"][:EMBED_MAX_CHARS] for t in buf]
        vecs = _embed_texts(args.embed_url, texts)
        recs = [_ingest_item(t, vecs[j]) for j, t in enumerate(buf)]
        resp = _post(url, {"records": recs})
        accepted += int(resp.get("accepted") or 0)
        skipped += int(resp.get("skipped") or 0)
        if resp.get("errors"):
            print("errors:", resp["errors"][:5], file=sys.stderr)
        buf = []

    for turn in walk_roots(args.pi, args.codex, args.host):
        n += 1
        if n <= args.skip:
            continue
        if turn["id"] in seen_ids:
            skipped += 1
            continue
        seen_ids.add(turn["id"])
        meta = turn["metadata"]
        if known_files.get(meta["file_path"]) == meta["file_hash"]:
            skipped += 1
            continue
        buf.append(turn)
        if args.limit and n >= args.skip + args.limit:
            break
        if len(buf) >= POST_BATCH:
            flush()
            print(f"posted {n} accepted={accepted} skipped={skipped}", file=sys.stderr)
    flush()
    print(f"done turns={n} accepted={accepted} skipped={skipped}")


def cmd_query(args: argparse.Namespace) -> None:
    body: dict = {"limit": args.limit, "scope": "session_memory"}
    if args.session:
        body["session_id"] = args.session
    if args.project:
        body["project_name"] = args.project
    if args.text:
        body["query_text"] = args.text
    if args.embed or not args.text:
        q = QUERY_PREFIX + (args.text or args.session or "")
        body["query_vector"] = _embed_texts(args.embed_url, [q])[0]
    resp = _post(args.server.rstrip("/") + "/v1/memory/query", body)
    print(json.dumps(resp, indent=2))


def cmd_session(args: argparse.Namespace) -> None:
    q = f"session_id={args.session_id}"
    if args.limit:
        q += f"&limit={args.limit}"
    url = args.server.rstrip("/") + "/v1/memory/session?" + q
    req = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            print(resp.read().decode())
    except urllib.error.HTTPError as e:
        raise SystemExit(f"HTTP {e.code}: {e.read().decode(errors='replace')}") from e


def _paths(ns: argparse.Namespace) -> None:
    home = Path.home()
    if ns.pi is None:
        ns.pi = Path(os.environ.get("TIRELESS_PI_SESSIONS", home / ".pi/agent/sessions"))
    if ns.codex is None:
        ns.codex = Path(os.environ.get("TIRELESS_CODEX_SESSIONS", home / ".codex/sessions"))
    ns.pi = Path(ns.pi) if ns.pi else None
    ns.codex = Path(ns.codex) if ns.codex else None
    if ns.host is None:
        ns.host = os.environ.get("TIRELESS_HOST") or socket.gethostname()


def main() -> None:
    p = argparse.ArgumentParser(prog="tireless-mac")
    p.add_argument("--server", default=os.environ.get("TIRELESS_SERVER", "http://127.0.0.1:8090"))
    p.add_argument("--host", default=None)
    p.add_argument("--embed-url", default=os.environ.get("TIRELESS_EMBED", DEFAULT_EMBED))
    p.add_argument("--pi", default=None)
    p.add_argument("--codex", default=None)
    sub = p.add_subparsers(dest="cmd", required=True)

    sp = sub.add_parser("parse", help="count/dump turns, no model")
    sp.add_argument("--limit", type=int, default=0)
    sp.add_argument("--dump", action="store_true")
    sp.set_defaults(func=cmd_parse)

    si = sub.add_parser("ingest", help="embed + POST /v1/memory/ingest")
    si.add_argument("--limit", type=int, default=0)
    si.add_argument("--skip", type=int, default=0, help="skip this many parsed turns before ingesting")
    si.add_argument(
        "--known-state",
        type=Path,
        default=None,
        help="skip file_path/file_hash pairs already present in a copied server dedup state",
    )
    si.add_argument("--dry-run", action="store_true")
    si.set_defaults(func=cmd_ingest)

    sq = sub.add_parser("query", help="BM25 and/or ANN query")
    sq.add_argument("text", nargs="?", default="")
    sq.add_argument("--session", default="")
    sq.add_argument("--project", default="")
    sq.add_argument("--limit", type=int, default=8)
    sq.add_argument("--embed", action="store_true", help="also send query_vector (hybrid)")
    sq.set_defaults(func=cmd_query)

    ss = sub.add_parser("session", help="list turns in one session")
    ss.add_argument("session_id")
    ss.add_argument("--limit", type=int, default=50)
    ss.set_defaults(func=cmd_session)

    args = p.parse_args()
    _paths(args)
    args.func(args)


if __name__ == "__main__":
    main()
