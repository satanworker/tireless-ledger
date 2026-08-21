from pathlib import Path

from parse import iter_codex_turns, iter_pi_turns, record_id

FIXTURE = Path(__file__).parent / "testdata"


def test_pi_user_and_assistant(tmp_path: Path) -> None:
    p = tmp_path / "2026-07-14T07-53-22-423Z_sess123.jsonl"
    p.write_text(
        "\n".join(
            [
                '{"type":"session","version":3,"id":"sess123","timestamp":"2026-07-14T07:53:22.423Z","cwd":"/Users/x/Developer/tireless-ledger"}',
                '{"type":"message","id":"u1","timestamp":"2026-07-14T07:53:36.796Z","message":{"role":"user","content":[{"type":"text","text":"is it a good solution for indexing codebases as well?"}]}}',
                '{"type":"message","id":"a1","timestamp":"2026-07-14T07:53:44.819Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"OK as shared vector store. Weak as full indexer."}]}}',
                '{"type":"message","id":"t1","timestamp":"2026-07-14T07:53:40Z","message":{"role":"toolResult","content":[{"type":"text","text":"ignore me please"}]}}',
            ]
        )
        + "\n"
    )
    turns = list(iter_pi_turns(p, "mac"))
    assert [t["metadata"]["role"] for t in turns] == ["user", "assistant"]
    assert turns[0]["metadata"]["session_id"] == "sess123"
    assert turns[0]["metadata"]["project_name"] == "tireless-ledger"
    assert "thinking" not in turns[1]["forward_content"]
    assert turns[0]["id"] == record_id("mac", "pi", "sess123", "u1")


def test_codex_skips_agents_md_and_dupes(tmp_path: Path) -> None:
    p = tmp_path / "rollout-2026-06-03T23-09-39-019e8f52-0cc9-7e91-a2bc-daea4b591f58.jsonl"
    user = "when I run unsloth studio, I cannot select Qwen"
    p.write_text(
        "\n".join(
            [
                '{"timestamp":"2026-06-03T21:09:41.734Z","type":"session_meta","payload":{"id":"019e8f52-0cc9-7e91-a2bc-daea4b591f58","cwd":"/Users/x/swap-ui"}}',
                '{"timestamp":"2026-06-03T21:09:42.850Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /tmp\\nskip"}]}}',
                '{"timestamp":"2026-06-03T21:09:42.857Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"%s"}]}}'
                % user,
                '{"timestamp":"2026-06-03T21:09:50Z","type":"event_msg","payload":{"type":"user_message","message":"%s"}}'
                % user,
                '{"timestamp":"2026-06-03T21:10:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Studio picker is GGUF only, your Qwen is MLX."}]}}',
            ]
        )
        + "\n"
    )
    turns = list(iter_codex_turns(p, "mac"))
    assert [t["metadata"]["role"] for t in turns] == ["user", "assistant"]
    assert turns[0]["metadata"]["session_id"] == "019e8f52-0cc9-7e91-a2bc-daea4b591f58"
    assert turns[0]["metadata"]["project_name"] == "swap-ui"
    assert "AGENTS.md" not in turns[0]["forward_content"]


if __name__ == "__main__":
    import tempfile

    with tempfile.TemporaryDirectory() as d:
        test_pi_user_and_assistant(Path(d))
        test_codex_skips_agents_md_and_dupes(Path(d))
    print("ok")
