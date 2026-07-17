#!/usr/bin/env python3
"""Regression: Claude Code → sub2api → grokcli-2api task status / terminal frames.

Root causes previously observed:
1. Bare error without message_delta+message_stop → agent hangs "running"
2. Incomplete Update/Edit shipped as "{}" → tool fails, status freezes
3. new_string="" treated incomplete → Update never ships / hangs
4. Responses complete() setting _closed on empty → fail() no-op, missing DONE
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))


def _parse_sse(frames: list[str]) -> list[dict]:
    payloads: list[dict] = []
    for e in frames:
        for line in e.splitlines():
            if line.startswith("data:"):
                raw = line[5:].strip()
                if not raw or raw == "[DONE]":
                    continue
                try:
                    payloads.append(json.loads(raw))
                except Exception:
                    pass
    return payloads


def main() -> int:
    from grok2api.protocol import anthropic_compat as a
    from grok2api.protocol import openai_responses as o

    print("=== tool readiness ===")
    cases = [
        ("Update", '{"file_path":"/x","old_string":"a","new_string":""}', True),
        ("Edit", '{"file_path":"/x","old_string":"a","new_string":""}', True),
        ("Update", '{"file_path":"/x","old_string":"a"}', False),
        ("Update", '{"file_path":"/x"}', False),
        ("Read", '{"file_path":""}', False),
        ("Read", '{"file_path":"/x"}', True),
        ("Bash", '{"command":"ls"}', True),
        ("Write", '{"file_path":"/x","content":""}', True),
        # Critical: Task*/Todo* must NOT inherit Update/Write required keys.
        ("TaskUpdate", '{"taskId":"1","status":"completed"}', True),
        ("TaskCreate", '{"subject":"x","description":"y"}', True),
        ("TodoWrite", '{"todos":[{"content":"a","status":"pending","activeForm":"doing"}]}', True),
        ("mcp__x__Update", '{"file_path":"/x","old_string":"a","new_string":"b"}', True),
        ("mcp__x__Update", '{"file_path":"/x"}', False),
        ("company_Update", '{"file_path":"/x","old_string":"a","new_string":"b"}', True),
    ]
    for name, args, expect in cases:
        got = a.is_complete_tool_arguments_json(args, tool_name=name)
        print(f"  {name:16} complete={got} expect={expect} raw={args[:70]}")
        assert got is expect, f"{name} readiness mismatch: {got} != {expect}"

    print("\n=== required-key suffix must not swallow TaskUpdate/TodoWrite ===")
    assert a._required_keys_for_tool("TaskUpdate") == ()
    assert a._required_keys_for_tool("TaskCreate") == ()
    assert a._required_keys_for_tool("TodoWrite") == ()
    assert a._required_keys_for_tool("Update") == (
        "file_path",
        "old_string",
        "new_string",
    )
    assert a._required_keys_for_tool("Write") == ("file_path", "content")
    assert a._required_keys_for_tool("mcp__x__Update") == (
        "file_path",
        "old_string",
        "new_string",
    )
    assert a._required_keys_for_tool("company_Update") == (
        "file_path",
        "old_string",
        "new_string",
    )
    print("  suffix boundary OK")

    print("\n=== terminal_error envelope ===")
    evs = a.anthropic_stream_terminal_error("boom")
    types = []
    for e in evs:
        et = None
        data = None
        for line in e.splitlines():
            if line.startswith("event:"):
                et = line.split(":", 1)[1].strip()
            if line.startswith("data:"):
                data = json.loads(line[5:].strip())
        stop = None
        if isinstance(data, dict):
            stop = (data.get("delta") or {}).get("stop_reason")
        types.append((et, data.get("type") if isinstance(data, dict) else None, stop))
    print("  types:", types)
    assert any(t[0] == "error" for t in types)
    assert any(t[0] == "message_delta" for t in types)
    assert any(t[0] == "message_stop" for t in types)
    assert any(t[2] == "end_turn" for t in types)
    print("  terminal_error OK")

    print("\n=== assembler: incomplete Update must NOT open / invent {} ===")
    asm = a.AnthropicStreamAssembler(
        message_id="msg_test1",
        model="grok-4.5",
        tools_requested=True,
        max_tools=1,
    )
    frames = asm.feed(
        tool_calls=[
            {
                "index": 0,
                "id": "toolu_test1",
                "function": {
                    "name": "Update",
                    "arguments": '{"file_path":"/x"}',
                },
            }
        ]
    )
    fin = asm.finish(finish_reason="tool_calls")
    payloads = _parse_sse(frames + fin)
    kinds = [p.get("type") for p in payloads]
    starts = [p for p in payloads if p.get("type") == "content_block_start"]
    print("  finish kinds:", kinds)
    print("  starts:", starts)
    assert any(p.get("type") == "message_stop" for p in payloads)
    assert any(p.get("type") == "message_delta" for p in payloads)
    assert not any(
        (p.get("content_block") or {}).get("name") == "Update" for p in starts
    ), "must not open incomplete Update"
    # No invented empty tool input
    deltas = [
        p
        for p in payloads
        if p.get("type") == "content_block_delta"
        and (p.get("delta") or {}).get("type") == "input_json_delta"
    ]
    assert not any((p.get("delta") or {}).get("partial_json") == "{}" for p in deltas)
    print("  incomplete Update OK")

    print("\n=== assembler: Update with new_string='' must ship ===")
    asm2 = a.AnthropicStreamAssembler(
        message_id="msg_test2",
        model="grok-4.5",
        tools_requested=True,
        max_tools=1,
    )
    args = '{"file_path":"/x","old_string":"a","new_string":""}'
    frames2 = asm2.feed(
        tool_calls=[
            {
                "index": 0,
                "id": "toolu_test2",
                "function": {"name": "Update", "arguments": args},
            }
        ]
    )
    fin2 = asm2.finish(finish_reason="tool_calls")
    payloads2 = _parse_sse(frames2 + fin2)
    starts2 = [p for p in payloads2 if p.get("type") == "content_block_start"]
    names2 = [(p.get("content_block") or {}).get("name") for p in starts2]
    print("  starts:", names2)
    # Empty allow-list still remaps Update → Edit (Claude Code never registers Update).
    assert "Edit" in names2, names2
    assert "Update" not in names2, names2
    assert any(p.get("type") == "message_stop" for p in payloads2)
    assert any(p.get("type") == "content_block_stop" for p in payloads2)
    # stop_reason should be tool_use
    deltas_msg = [p for p in payloads2 if p.get("type") == "message_delta"]
    assert deltas_msg, "need message_delta"
    stop = (deltas_msg[-1].get("delta") or {}).get("stop_reason")
    print("  stop_reason:", stop)
    assert stop == "tool_use"
    print("  empty new_string Update OK")

    print("\n=== assembler: TaskUpdate complete ships + terminal ===")
    asm3 = a.AnthropicStreamAssembler(
        message_id="msg_test3",
        model="grok-4.5",
        tools_requested=True,
        max_tools=1,
    )
    targs = '{"taskId":"1","status":"completed"}'
    frames3 = asm3.feed(
        tool_calls=[
            {
                "index": 0,
                "id": "toolu_task",
                "function": {"name": "TaskUpdate", "arguments": targs},
            }
        ]
    )
    fin3 = asm3.finish(finish_reason="tool_calls")
    payloads3 = _parse_sse(frames3 + fin3)
    starts3 = [
        (p.get("content_block") or {}).get("name")
        for p in payloads3
        if p.get("type") == "content_block_start"
    ]
    print("  starts:", starts3)
    assert "TaskUpdate" in starts3
    assert any(p.get("type") == "message_stop" for p in payloads3)
    print("  TaskUpdate terminal OK")

    print("\n=== ResponsesLiveStreamer: empty complete leaves fail() usable ===")
    s = o.ResponsesLiveStreamer(response_id="resp_test", model="grok-4.5")
    s.start()
    empty = s.complete(usage={"prompt_tokens": 1, "completion_tokens": 0})
    print("  empty complete frames:", len(empty), "closed:", s._closed)
    assert empty == []
    assert s._closed is False
    fail = s.fail("empty upstream")
    print("  fail frames:", len(fail))
    assert any("response.failed" in f for f in fail)
    assert any("[DONE]" in f for f in fail)
    assert s._closed is True
    print("  empty complete → fail OK")

    print("\n=== ResponsesLiveStreamer: Update new_string='' ships completed ===")
    s2 = o.ResponsesLiveStreamer(response_id="resp_test2", model="grok-4.5")
    s2.start()
    s2.on_tool_delta(
        [
            {
                "index": 0,
                "id": "call_1",
                "function": {
                    "name": "Update",
                    "arguments": '{"file_path":"/x","old_string":"a","new_string":""}',
                },
            }
        ]
    )
    done = s2.complete(usage={"prompt_tokens": 10, "completion_tokens": 5})
    print("  done frames:", len(done), "closed:", s2._closed)
    assert s2._closed
    assert any("response.completed" in f for f in done)
    assert any("[DONE]" in f for f in done)
    print("  Responses Update empty new_string OK")

    print("\n=== ResponsesLiveStreamer: TaskUpdate ships completed (sub2api path) ===")
    s2b = o.ResponsesLiveStreamer(response_id="resp_test2b", model="grok-4.5")
    s2b.start()
    mid = s2b.on_tool_delta(
        [
            {
                "index": 0,
                "id": "call_task",
                "function": {
                    "name": "TaskUpdate",
                    "arguments": '{"taskId":"1","status":"completed"}',
                },
            }
        ]
    )
    done2b = s2b.complete(usage={"prompt_tokens": 10, "completion_tokens": 5})
    all2b = mid + done2b
    print("  TaskUpdate frames:", len(all2b), "closed:", s2b._closed)
    assert s2b._closed
    assert any("response.completed" in f for f in all2b)
    assert any("[DONE]" in f for f in all2b)
    # function_call item should appear (not held forever)
    assert any("function_call" in f for f in all2b)
    print("  Responses TaskUpdate OK")

    print("\n=== ResponsesLiveStreamer: incomplete Update does not complete empty ===")
    s3 = o.ResponsesLiveStreamer(response_id="resp_test3", model="grok-4.5")
    s3.start()
    s3.on_tool_delta(
        [
            {
                "index": 0,
                "id": "call_2",
                "function": {
                    "name": "Update",
                    "arguments": '{"file_path":"/x"}',
                },
            }
        ]
    )
    empty3 = s3.complete(
        usage={"prompt_tokens": 1, "completion_tokens": 0},
        force_flush_partial_tools=False,
    )
    print("  incomplete complete frames:", len(empty3), "closed:", s3._closed)
    # Should not emit empty completed; leave reopenable for fail
    assert empty3 == [] or not any("response.completed" in f for f in empty3)
    if empty3 == []:
        assert s3._closed is False
        fail3 = s3.fail("incomplete tool")
        assert any("response.failed" in f for f in fail3)
        assert any("[DONE]" in f for f in fail3)
    print("  incomplete Update empty-path OK")

    print("\n=== Update file_path beats path alias ===")
    test_update_file_path_beats_path_alias()
    print("  file_path beats path OK")

    print("\n=== Update→Edit remapping (Claude Code) ===")
    test_update_to_edit_remap_all_paths()
    print("  Update→Edit remapping OK")

    print("\n=== soft-disconnect body frames ===")
    test_soft_disconnect_does_not_drop_body_frames()
    print("  soft-disconnect body frames OK")

    print("\n=== codex reasoning leak ===")
    test_codex_reasoning_not_leaked_as_output_text()
    print("  codex reasoning not leaked OK")

    print("\nALL PASS")
    return 0





def test_update_file_path_beats_path_alias():
    """Claude Code → sub2api → grokcli-2api: Update/Edit must not open the wrong file.

    Stream merges and doubled JSON often carry both path (OpenAI/Cursor style)
    and file_path (Claude Code schema). Later complete rewrites must win over
    stale early path previews (including when the early key was already
    canonical file_path and the later key is only path).
    """
    import json
    from grok2api.protocol import anthropic_compat as anth

    # 1) Single object: alias first, then canonical (same value) → keep file_path
    n_same = anth.normalize_tool_argument_keys(
        {"path": "/same", "file_path": "/same", "old_string": "a", "new_string": "b"}
    )
    assert n_same["file_path"] == "/same", n_same
    assert "path" not in n_same, n_same

    # 1b) Different values in one object: later key wins (insertion order)
    n = anth.normalize_tool_argument_keys(
        {"path": "/wrong", "file_path": "/correct", "old_string": "a", "new_string": "b"}
    )
    assert n["file_path"] == "/correct", n
    assert "path" not in n, n

    n2 = anth.normalize_tool_argument_keys(
        {"file_path": "/wrong", "path": "/correct", "old_string": "a", "new_string": "b"}
    )
    assert n2["file_path"] == "/correct", n2

    # 3) Stream merge: incomplete path-only first, later complete rewrite wins
    #    (including path under alias). Early path-only previews are often wrong
    #    and must not stick over a later complete Update/Edit payload.
    merged = anth.merge_tool_argument_delta(
        '{"file_path":"/stale-preview"}',
        '{"path":"/correct","old_string":"a","new_string":"b"}',
        tool_name="Update",
    )
    obj = json.loads(merged)
    assert obj.get("file_path") == "/correct", merged
    assert obj.get("old_string") == "a", merged
    assert obj.get("new_string") == "b", merged
    assert "path" not in obj, merged

    # 3b) Complete early payload must not be clobbered by later incomplete path
    merged_keep = anth.merge_tool_argument_delta(
        '{"file_path":"/correct","old_string":"a","new_string":"b"}',
        '{"path":"/wrong"}',
        tool_name="Update",
    )
    obj_keep = json.loads(merged_keep)
    assert obj_keep.get("file_path") == "/correct", merged_keep
    assert obj_keep.get("old_string") == "a", merged_keep
    assert obj_keep.get("new_string") == "b", merged_keep

    # 3c) BOTH complete: later path rewrite must replace early wrong file_path
    #     (the intermittent "Error editing file" path after sub2api).
    merged_both = anth.merge_tool_argument_delta(
        '{"file_path":"/wrong","old_string":"a","new_string":"b"}',
        '{"path":"/correct","old_string":"a","new_string":"c"}',
        tool_name="Update",
    )
    obj_both = json.loads(merged_both)
    assert obj_both.get("file_path") == "/correct", merged_both
    assert obj_both.get("new_string") == "c", merged_both

    # 4) Stream merge opposite order: path first, then correct file_path rewrite
    merged2 = anth.merge_tool_argument_delta(
        '{"path":"/wrong"}',
        '{"file_path":"/correct","old_string":"a","new_string":"b"}',
        tool_name="Update",
    )
    obj2 = json.loads(merged2)
    assert obj2.get("file_path") == "/correct", merged2

    # 5) Doubled blob in one chunk
    san = anth.sanitize_tool_arguments_json(
        '{"path":"/wrong"}{"file_path":"/correct","old_string":"a","new_string":"b"}',
        tool_name="Update",
    )
    san_obj = json.loads(anth.normalize_tool_arguments_json(san, tool_name="Update"))
    assert san_obj.get("file_path") == "/correct", san_obj

    # 5b) Intermittent failure: early wrong file_path + later complete rewrite via path
    san_flip = anth.sanitize_tool_arguments_json(
        '{"file_path":"/wrong"}{"path":"/correct","old_string":"a","new_string":"b"}',
        tool_name="Update",
    )
    san_flip_obj = json.loads(
        anth.normalize_tool_arguments_json(san_flip, tool_name="Update")
    )
    assert san_flip_obj.get("file_path") == "/correct", san_flip_obj
    assert san_flip_obj.get("old_string") == "a", san_flip_obj

    # 5c) Doubled BOTH complete: later path body must win
    san_both = anth.sanitize_tool_arguments_json(
        '{"file_path":"/wrong","old_string":"a","new_string":"b"}'
        '{"path":"/correct","old_string":"a","new_string":"c"}',
        tool_name="Update",
    )
    san_both_obj = json.loads(
        anth.normalize_tool_arguments_json(san_both, tool_name="Update")
    )
    assert san_both_obj.get("file_path") == "/correct", san_both_obj
    assert san_both_obj.get("new_string") == "c", san_both_obj

    # 5d) Stream merge: partial wrong path then complete rewrite under alias.
    m_flip = anth.merge_tool_argument_delta(
        '{"file_path":"/wrong"}',
        '{"path":"/correct","old_string":"old","new_string":"new"}',
        tool_name="Update",
    )
    m_flip_obj = json.loads(m_flip)
    assert m_flip_obj.get("file_path") == "/correct", m_flip_obj
    assert m_flip_obj.get("old_string") == "old", m_flip_obj

    # 5e) target_file alias (Cursor / Codex style)
    n_tf = anth.normalize_tool_argument_keys(
        {"target_file": "/via-target", "old_string": "a", "new_string": "b"}
    )
    assert n_tf.get("file_path") == "/via-target", n_tf
    assert "target_file" not in n_tf, n_tf

    # 6) Empty canonical should not block a non-empty alias
    n3 = anth.normalize_tool_argument_keys({"file_path": "", "path": "/only-alias"})
    assert n3.get("file_path") == "/only-alias", n3

    # 7) openai_responses local mirror agrees
    from grok2api.protocol import openai_responses as oresp
    ln = oresp._local_normalize_tool_arg_keys(
        {"path": "/wrong", "file_path": "/correct"}
    )
    assert ln.get("file_path") == "/correct", ln

    ln2 = oresp._local_normalize_tool_arg_keys(
        {"file_path": "/wrong", "path": "/correct"}
    )
    assert ln2.get("file_path") == "/correct", ln2

    # 7b) local doubled blob flip (wrong early file_path, later complete path)
    san_local = oresp._local_sanitize_tool_arguments_json(
        '{"file_path":"/wrong"}{"path":"/correct","old_string":"a","new_string":"b"}',
        tool_name="Update",
    )
    san_local_obj = json.loads(
        oresp._local_normalize_tool_arguments_json(san_local, tool_name="Update")
    )
    assert san_local_obj.get("file_path") == "/correct", san_local_obj

    # 7c) local doubled both-complete
    san_local_both = oresp._local_sanitize_tool_arguments_json(
        '{"file_path":"/wrong","old_string":"a","new_string":"b"}'
        '{"path":"/correct","old_string":"a","new_string":"c"}',
        tool_name="Update",
    )
    san_local_both_obj = json.loads(
        oresp._local_normalize_tool_arguments_json(san_local_both, tool_name="Update")
    )
    assert san_local_both_obj.get("file_path") == "/correct", san_local_both_obj
    assert san_local_both_obj.get("new_string") == "c", san_local_both_obj

    print("test_update_file_path_beats_path_alias OK")


def test_update_to_edit_remap_all_paths():
    """Claude Code only registers Edit; Grok often invents Update/StrReplace.

    All outbound surfaces (Responses live/terminal, Anthropic, OpenAI chat)
    must rewrite Update→Edit. Empty allow-list still remaps (sub2api often
    fails to forward the tools array).
    """
    from grok2api.protocol import anthropic_compat as anth
    from grok2api.protocol import openai_responses as oresp

    # 1) pure remapper
    assert anth.canonical_outbound_tool_name("Update", allowed_names=None) == "Edit"
    assert anth.canonical_outbound_tool_name("Update", allowed_names=set()) == "Edit"
    assert anth.canonical_outbound_tool_name(
        "Update", allowed_names={"Edit", "Read", "Write", "Bash"}
    ) == "Edit"
    assert anth.canonical_outbound_tool_name(
        "StrReplace", allowed_names={"Edit"}
    ) == "Edit"
    assert anth.canonical_outbound_tool_name(
        "TaskUpdate", allowed_names={"Edit", "TaskUpdate"}
    ) == "TaskUpdate"
    # Custom agent that only registered Update keeps it.
    assert anth.canonical_outbound_tool_name(
        "Update", allowed_names={"Update"}
    ) == "Update"

    full_args = (
        '{"file_path":"/tmp/x.py","old_string":"old","new_string":"new"}'
    )

    # 2) Responses live streamer mid-stream
    s = oresp.ResponsesLiveStreamer(
        response_id="resp_t1",
        model="grok-4.5",
        allowed_tool_names={"Edit", "Read", "Write", "Bash"},
    )
    s.start()
    frames = s.on_tool_delta(
        [
            {
                "index": 0,
                "id": "call_u1",
                "function": {"name": "Update", "arguments": full_args},
            }
        ]
    )
    frames += s.complete(usage={"prompt_tokens": 1, "completion_tokens": 1})
    names = _collect_function_names(frames)
    assert names, "expected function_call frames"
    assert all(n == "Edit" for n in names), names

    # 3) Responses terminal-only path (args complete only at close)
    s2 = oresp.ResponsesLiveStreamer(
        response_id="resp_t2",
        model="grok-4.5",
        allowed_tool_names={"Edit", "Read"},
    )
    s2.start()
    s2.on_tool_delta(
        [
            {
                "index": 0,
                "id": "call_u2",
                "function": {
                    "name": "Update",
                    "arguments": '{"file_path":"/tmp/x.py","old_string":"a"}',
                },
            }
        ]
    )
    assert 0 not in s2._tool_opened
    # Name is remapped on first merge, even while args are incomplete.
    assert (s2._tools.get(0) or {}).get("name") == "Edit", s2._tools.get(0)
    # Defensive: if a hop still held raw "Update" until terminal close, remap there.
    s2._tools[0]["name"] = "Update"
    s2._tools[0]["arguments"] = full_args
    s2._tools[0]["args_emitted"] = False
    s2._tools[0]["output_index"] = None
    s2._tool_opened.clear()
    s2._tool_done.clear()
    frames2 = s2.complete(usage={"prompt_tokens": 1, "completion_tokens": 1})
    names2 = _collect_function_names(frames2)
    assert (s2._tools.get(0) or {}).get("name") == "Edit", s2._tools.get(0)
    assert names2 and all(n == "Edit" for n in names2), names2

    # 4) empty allow-list still remaps
    s3 = oresp.ResponsesLiveStreamer(response_id="resp_t3", model="g")
    s3.start()
    frames3 = s3.on_tool_delta(
        [
            {
                "index": 0,
                "id": "call_u3",
                "function": {"name": "Update", "arguments": full_args},
            }
        ]
    )
    frames3 += s3.complete(usage={"prompt_tokens": 1, "completion_tokens": 1})
    names3 = _collect_function_names(frames3)
    assert names3 and all(n == "Edit" for n in names3), names3

    # 5) Anthropic assembler
    asm = anth.AnthropicStreamAssembler(
        message_id="msg_t",
        model="grok-4.5",
        tools_requested=True,
        max_tools=1,
        allowed_tool_names={"Edit", "Read"},
    )
    frames_a = asm.feed(
        tool_calls=[
            {
                "index": 0,
                "id": "toolu_1",
                "function": {"name": "Update", "arguments": full_args},
            }
        ]
    )
    frames_a += asm.finish(finish_reason="tool_calls")
    anames = _collect_anthropic_tool_names(frames_a)
    assert anames and all(n == "Edit" for n in anames), anames

    # 6) OpenAI chat outbound builder
    import importlib

    application = importlib.import_module("grok2api.app")

    # Bind allow-list like chat_completions does
    try:
        application._allowed_tool_names_ctx.set({"Edit", "Read", "Write", "Bash"})
    except Exception:
        pass
    acc: dict = {}
    application._ingest_tool_call_deltas(
        acc,
        [
            {
                "index": 0,
                "id": "call_chat1",
                "function": {"name": "Update", "arguments": full_args},
            }
        ],
    )
    flushed = application._flush_one_tool_call(acc)
    assert flushed, "expected flushed tool"
    assert flushed[0]["function"]["name"] == "Edit", flushed[0]
    # finalize path
    acc2: dict = {}
    application._ingest_tool_call_deltas(
        acc2,
        [
            {
                "index": 0,
                "id": "call_chat2",
                "function": {"name": "StrReplace", "arguments": full_args},
            }
        ],
    )
    fin = application._finalize_tool_calls(acc2)
    assert fin and fin[0]["function"]["name"] == "Edit", fin
    print("test_update_to_edit_remap_all_paths OK")




def test_soft_disconnect_does_not_drop_body_frames():
    """False-positive client_gone must not drop mid-stream tool/text frames.

    Historical bug: soft-disconnect probe latched under backpressure, then
    Responses/Anthropic paths skipped body yields while still mutating streamer
    state. Terminal arrived without matching function_call/tool_use → Claude
    Code hard-cut ("stream interrupted").
    """
    import asyncio
    import grok2api.app as application
    from grok2api.protocol import openai_responses as oresp
    from grok2api.protocol import anthropic_compat as anth

    full_args = (
        '{"file_path":"/tmp/x.py","old_string":"old","new_string":"new"}'
    )

    # OpenAI chat: even if client_gone would have been true, merge+flush still
    # produces Edit tool items (frame emission is now ungated at call sites).
    application._allowed_tool_names_ctx.set({"Edit", "Read", "Write", "Bash"})
    acc = {}
    application._ingest_tool_call_deltas(
        acc,
        [
            {
                "index": 0,
                "id": "call_sd1",
                "function": {"name": "Update", "arguments": full_args},
            }
        ],
    )
    flushed = application._flush_one_tool_call(acc)
    assert flushed and flushed[0]["function"]["name"] == "Edit"

    # Responses streamer: mid frames exist and complete still has tools.
    s = oresp.ResponsesLiveStreamer(
        response_id="resp_sd",
        model="g",
        allowed_tool_names={"Edit"},
    )
    s.start()
    mid = s.on_tool_delta(
        [
            {
                "index": 0,
                "id": "call_sd2",
                "function": {"name": "Update", "arguments": full_args},
            }
        ]
    )
    assert mid, "mid tool frames must exist before complete"
    assert s.has_client_payload()
    done = s.complete(usage={"prompt_tokens": 1, "completion_tokens": 1})
    names = _collect_function_names(mid + done)
    assert names and all(n == "Edit" for n in names), names

    # Anthropic serial helper must yield even when client_gone=True.
    async def _run():
        events = [
            'event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Edit","input":{}}}\n\n',
            'event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n',
        ]
        out = []
        async for ev in application._yield_anthropic_events_serial(
            events, client_gone=True, protocol="anthropic"
        ):
            out.append(ev)
        return out

    out = asyncio.run(_run())
    assert len(out) == 2, out
    print("test_soft_disconnect_does_not_drop_body_frames OK")



def test_codex_reasoning_not_leaked_as_output_text():
    """Codex must not see model monologue as assistant output_text.

    v1.9.73 streamed reasoning via streamer.on_text_delta(reasoning) for
    openai-native clients. That made short turns (hello) print the full
    thinking chain in the Codex UI. Reasoning stays internal.
    """
    import re
    from pathlib import Path

    app_src = ROOT / "grok2api" / "app.py"
    src = app_src.read_text(encoding="utf-8")
    assert "reasoning_as_text" not in src, "reasoning_as_text path must be removed"
    # Mid-stream must not call on_text_delta(reasoning)
    assert not re.search(r"on_text_delta\(\s*reasoning\s*\)", src), src
    assert not re.search(r"on_text_delta\(\s*joined_reasoning\s*\)", src), src
    # Keepalive on reasoning still present (held TTFT diagnostic)
    assert 'kind="reasoning"' in src and "held=True" in src
    print("test_codex_reasoning_not_leaked_as_output_text OK")

def _collect_function_names(frames):
    import json

    names = []
    for f in frames or []:
        for line in str(f).splitlines():
            if not line.startswith("data:"):
                continue
            raw = line[5:].strip()
            if not raw or raw == "[DONE]":
                continue
            try:
                p = json.loads(raw)
            except Exception:
                continue
            item = p.get("item") or {}
            if isinstance(item, dict) and item.get("name"):
                names.append(item["name"])
            if p.get("type") == "response.completed":
                for it in (p.get("response") or {}).get("output") or []:
                    if isinstance(it, dict) and it.get("type") == "function_call" and it.get("name"):
                        names.append(it["name"])
    return names


def _collect_anthropic_tool_names(frames):
    import json

    names = []
    for f in frames or []:
        for line in str(f).splitlines():
            if not line.startswith("data:"):
                continue
            raw = line[5:].strip()
            if not raw or raw == "[DONE]":
                continue
            try:
                p = json.loads(raw)
            except Exception:
                continue
            if p.get("type") == "content_block_start":
                cb = p.get("content_block") or {}
                if cb.get("type") == "tool_use" and cb.get("name"):
                    names.append(cb["name"])
    return names


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AssertionError as e:
        print("ASSERT FAIL:", e, file=sys.stderr)
        raise
    except Exception as e:
        print("ERROR:", type(e).__name__, e, file=sys.stderr)
        raise

