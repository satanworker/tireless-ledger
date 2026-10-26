#!/usr/bin/env python3
"""Export Telegram messages to <out>/<chat_id>/<yyyy-mm>.jsonl for tireless-upload --telegram.

Run on hosts via: telethon-python cmd/tireless-telegram/tireless_telegram.py [flags]
First run logs in interactively (phone + code); later runs reuse --session.
"""
import argparse
import asyncio
import datetime
import json
import os
import sys

STATE_DIR = os.path.expanduser("~/.local/state/tireless-telegram")
TYPES = ("dm", "group", "topic", "channel")


def classify(entity_kind, forum_topic, reply_msg_id, reply_top_id):
    """Return (chat_type, topic_id, reply_to) for a message in a dm/group/channel."""
    if entity_kind == "group" and forum_topic:
        # In a topic the bare reply header points at the topic root; a real reply also sets top id.
        return "topic", reply_top_id or reply_msg_id, reply_msg_id if reply_top_id else None
    return entity_kind, None, reply_msg_id


def build_line(msg_id, chat_id, chat_type, chat_title, topic_id, date, edit_date,
               sender_id, sender_name, reply_to, fwd_from, media, text):
    """One contract line: ids as strings, dates as unix seconds, ""/0 when absent."""
    def s(v):
        return "" if v is None else str(v)
    return {
        "id": s(msg_id), "chat_id": s(chat_id), "chat_type": chat_type, "chat_title": s(chat_title),
        "topic_id": s(topic_id), "date": int(date or 0), "edit_date": int(edit_date or 0),
        "sender_id": s(sender_id), "sender_name": s(sender_name), "reply_to": s(reply_to),
        "fwd_from": s(fwd_from), "media": s(media), "text": s(text),
    }


def month_path(out, chat_id, date):
    month = datetime.datetime.fromtimestamp(date, datetime.timezone.utc).strftime("%Y-%m")
    return os.path.join(out, str(chat_id), month + ".jsonl")


def load_state(path):
    try:
        with open(path) as f:
            return json.load(f)
    except FileNotFoundError:
        return {}


def save_state(path, state):
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f, indent=1, sort_keys=True)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def load_allowlist(path):
    ids = set()
    with open(path) as f:
        for line in f:
            line = line.split("#", 1)[0].strip()
            if line:
                ids.add(int(line))
    return ids


def entity_kind(entity):
    from telethon.tl.types import Channel, User
    if isinstance(entity, User):
        return "dm"
    if isinstance(entity, Channel) and entity.broadcast:
        return "channel"
    return "group"


def display_name(entity):
    from telethon.utils import get_display_name
    if entity is None:
        return ""
    return get_display_name(entity) or getattr(entity, "username", None) or ""


def message_line(msg, chat_id, kind, title):
    reply = msg.reply_to
    chat_type, topic_id, reply_to = classify(
        kind, bool(reply and getattr(reply, "forum_topic", False)),
        getattr(reply, "reply_to_msg_id", None), getattr(reply, "reply_to_top_id", None))
    fwd = ""
    if msg.forward:
        fwd = display_name(msg.forward.chat or msg.forward.sender) or msg.forward.from_name or ""
    media = type(msg.media).__name__.removeprefix("MessageMedia").lower() if msg.media else ""
    return build_line(
        msg.id, chat_id, chat_type, title, topic_id, msg.date.timestamp(),
        msg.edit_date.timestamp() if msg.edit_date else 0, msg.sender_id,
        display_name(msg.sender), reply_to, fwd, media, msg.message)


async def export_chat(client, dialog, args, state, types):
    chat_id = str(dialog.id)
    kind, title = entity_kind(dialog.entity), display_name(dialog.entity)
    cursor = state.get(chat_id, 0)
    since = None
    if not cursor and args.since:
        since = datetime.datetime.strptime(args.since, "%Y-%m-%d").replace(tzinfo=datetime.timezone.utc)
    # ponytail: only messages newer than the cursor are fetched, so edits/deletes of already
    # exported messages are never seen; re-scan the last N days (append edit lines) if that matters.
    path, out, written = None, None, 0
    try:
        async for msg in client.iter_messages(dialog.entity, min_id=cursor, offset_date=since, reverse=True):
            line = None if getattr(msg, "action", None) is not None else message_line(msg, chat_id, kind, title)
            if line and line["chat_type"] in types:
                p = month_path(args.out, chat_id, line["date"])
                if p != path:
                    if out:
                        out.close()
                    os.makedirs(os.path.dirname(p), exist_ok=True)
                    path, out = p, open(p, "a")
                out.write(json.dumps(line, ensure_ascii=False) + "\n")
                written += 1
            cursor = msg.id
    finally:
        if out:
            out.close()
        if cursor:
            state[chat_id] = cursor
            save_state(args.state, state)
    return written


async def run(args):
    from telethon import TelegramClient
    from telethon.errors import FloodWaitError

    api_id, api_hash = os.environ.get("TELEGRAM_API_ID"), os.environ.get("TELEGRAM_API_HASH")
    if not api_id or not api_hash:
        sys.exit("TELEGRAM_API_ID and TELEGRAM_API_HASH are required")
    types = set(args.types.split(","))
    if not types <= set(TYPES):
        sys.exit(f"--types must be a subset of {','.join(TYPES)}")
    allow = None if args.chats == "all" else load_allowlist(args.chats)
    os.makedirs(os.path.dirname(args.session) or ".", exist_ok=True)
    state = load_state(args.state)

    client = TelegramClient(args.session, int(api_id), api_hash)
    await client.start()
    try:
        total = 0
        async for dialog in client.iter_dialogs():
            if allow is not None and dialog.id not in allow:
                continue
            kind = entity_kind(dialog.entity)
            if kind not in types and not (kind == "group" and "topic" in types):
                continue
            while True:
                try:
                    n = await export_chat(client, dialog, args, state, types)
                    break
                except FloodWaitError as e:
                    print(f"flood wait {e.seconds}s on {dialog.id}", file=sys.stderr)
                    await asyncio.sleep(e.seconds)
            total += n
            if n:
                print(f"{dialog.id} {display_name(dialog.entity)!r}: {n}")
        print(f"exported={total}")
    finally:
        await client.disconnect()


def self_check():
    line = build_line(812, -1001234567890, "group", "Valantis Core", None, 1791311374.9, None,
                      55501, "Alice", 809, None, None, "pushed v0.3 to staging")
    want = ('{"id":"812","chat_id":"-1001234567890","chat_type":"group","chat_title":"Valantis Core",'
            '"topic_id":"","date":1791311374,"edit_date":0,"sender_id":"55501","sender_name":"Alice",'
            '"reply_to":"809","fwd_from":"","media":"","text":"pushed v0.3 to staging"}')
    got = json.dumps(line, separators=(",", ":"))
    assert got == want, got
    assert month_path("/x", -1001234567890, 1791311374) == "/x/-1001234567890/2026-10.jsonl"
    assert month_path("/x", 1, 1767225599) == "/x/1/2025-12.jsonl"  # 2025-12-31T23:59:59Z, UTC month
    assert classify("dm", False, None, None) == ("dm", None, None)
    assert classify("channel", False, 5, None) == ("channel", None, 5)
    assert classify("group", False, 7, None) == ("group", None, 7)  # forum General topic / plain group
    assert classify("group", True, 40, None) == ("topic", 40, None)  # top-level message in topic 40
    assert classify("group", True, 45, 40) == ("topic", 40, 45)  # reply to 45 inside topic 40
    print("self-check ok")


def main():
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--session", default=os.path.join(STATE_DIR, "account.session"), help="Telethon session file")
    p.add_argument("--out", default=os.path.join(STATE_DIR, "export"), help="export dir (<out>/<chat_id>/<yyyy-mm>.jsonl)")
    p.add_argument("--state", default=os.path.join(STATE_DIR, "state.json"), help="per-chat cursor state JSON")
    p.add_argument("--chats", default="all", help="'all' or a file of chat ids, one per line (# comments)")
    p.add_argument("--types", default=",".join(TYPES), help="comma list of dm,group,topic,channel")
    p.add_argument("--since", help="YYYY-MM-DD limit for the first backfill of a chat (default: full history)")
    p.add_argument("--self-check", action="store_true", help="run built-in checks and exit (no Telethon needed)")
    args = p.parse_args()
    if args.self_check:
        self_check()
        return
    if args.since:
        datetime.datetime.strptime(args.since, "%Y-%m-%d")
    for k in ("session", "out", "state"):
        setattr(args, k, os.path.expanduser(getattr(args, k)))
    asyncio.run(run(args))


if __name__ == "__main__":
    main()
