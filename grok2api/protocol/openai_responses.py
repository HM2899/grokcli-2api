"""OpenAI Responses API compatibility helpers for grokcli-2api.

sub2api (platform=openai, Anthropic inbound /v1/messages) forwards to upstream
as POST /v1/responses with stream=true. This module converts:

  Responses request  →  OpenAI chat/completions body
  chat completion    →  Responses object / SSE events

We intentionally keep the surface small: enough for Claude Code via sub2api
(text + function tools + usage + terminal response.completed).
"""

from __future__ import annotations

import json
import time
import uuid
from typing import Any


def new_response_id() -> str:
    return f"resp_{uuid.uuid4().hex[:24]}"


def new_output_item_id(prefix: str = "msg") -> str:
    return f"{prefix}_{uuid.uuid4().hex[:20]}"


def _stringify(v: Any) -> str:
    if v is None:
        return ""
    if isinstance(v, str):
        return v
    try:
        return json.dumps(v, ensure_ascii=False)
    except (TypeError, ValueError):
        return str(v)


# Local mirror of anthropic_compat._TOOL_REQUIRED_KEYS for the import-failure
# fallback in ResponsesLiveStreamer._args_ready. Keep in sync when adding tools.
_LOCAL_TOOL_REQUIRED_KEYS: dict[str, tuple[str, ...]] = {
    "read": ("file_path",),
    "write": ("file_path", "content"),
    "edit": ("file_path", "old_string", "new_string"),
    "update": ("file_path", "old_string", "new_string"),
    "strreplace": ("file_path", "old_string", "new_string"),
    "str_replace": ("file_path", "old_string", "new_string"),
    "stringreplace": ("file_path", "old_string", "new_string"),
    "replace": ("file_path", "old_string", "new_string"),
    "multiedit": ("file_path", "edits"),
    "notebookedit": ("notebook_path", "new_source"),
    "bash": ("command",),
    "shell": ("command",),
    "grep": ("pattern",),
    "glob": ("pattern",),
    "webfetch": ("url",),
    "websearch": ("query",),
    "web_search": ("query",),
}

_LOCAL_TOOL_ARG_KEY_ALIASES: dict[str, str] = {
    "path": "file_path",
    "filepath": "file_path",
    "file": "file_path",
    "filename": "file_path",
    # Cursor / Codex / some Grok variants
    "target_file": "file_path",
    "targetfile": "file_path",
    "targetpath": "file_path",
    "target_path": "file_path",
    "file_name": "file_path",
    "oldstring": "old_string",
    "oldstr": "old_string",
    "oldtext": "old_string",
    "old": "old_string",
    "old_text": "old_string",
    "original": "old_string",
    "original_text": "old_string",
    "newstring": "new_string",
    "newstr": "new_string",
    "newtext": "new_string",
    "new": "new_string",
    "new_text": "new_string",
    "replacement": "new_string",
    "replace_with": "new_string",
    "notebookpath": "notebook_path",
    "notebook": "notebook_path",
    "cmd": "command",
    "shell_command": "command",
    "q": "query",
    "search": "query",
    "search_query": "query",
    "uri": "url",
    "href": "url",
    "regex": "pattern",
    "glob_pattern": "pattern",
}


def _local_tool_name_key(name: str | None) -> str:
    import re

    return re.sub(r"[^a-z0-9_]+", "", (name or "").strip().lower())


def _local_required_keys_for_tool(name: str | None) -> tuple[str, ...]:
    key = _local_tool_name_key(name)
    if not key:
        return ()
    if key in _LOCAL_TOOL_REQUIRED_KEYS:
        return _LOCAL_TOOL_REQUIRED_KEYS[key]
    # Token-boundary only — never plain endswith. TaskUpdate must not inherit
    # Update's file_path/old_string/new_string requirements (same for TodoWrite
    # vs Write). That mis-match held task tools forever under sub2api.
    for short, req in _LOCAL_TOOL_REQUIRED_KEYS.items():
        if key.endswith(f"_{short}") or key.endswith(f"__{short}"):
            return req
    return ()


def _local_canonical_tool_arg_key(key: str) -> str:
    import re

    raw = str(key or "").strip()
    if not raw:
        return raw
    folded_keep_us = re.sub(r"[^a-z0-9_]+", "", raw.lower())
    folded_alnum = folded_keep_us.replace("_", "")
    if folded_keep_us in _LOCAL_TOOL_ARG_KEY_ALIASES:
        return _LOCAL_TOOL_ARG_KEY_ALIASES[folded_keep_us]
    if folded_alnum in _LOCAL_TOOL_ARG_KEY_ALIASES:
        return _LOCAL_TOOL_ARG_KEY_ALIASES[folded_alnum]
    return raw


def _local_normalize_tool_arg_keys(obj: Any) -> Any:
    """Mirror of anthropic_compat.normalize_tool_argument_keys.

    Preference when several keys collapse to the same canonical name:
    1. Non-empty beats empty.
    2. Equal non-empty values → prefer already-canonical spelling.
    3. Different non-empty values → **later wins** (stale early file_path must
       not beat a later complete path rewrite from sub2api / Grok streams).
    """
    if not isinstance(obj, dict):
        return obj

    def _empty(value: Any) -> bool:
        return value in (None, "", [], {})

    chosen: dict[str, tuple[Any, bool]] = {}
    for k, v in obj.items():
        raw = str(k)
        canon = _local_canonical_tool_arg_key(raw)
        from_canon = raw == canon
        if canon not in chosen:
            chosen[canon] = (v, from_canon)
            continue
        old_v, old_canon = chosen[canon]
        if _empty(old_v) and not _empty(v):
            chosen[canon] = (v, from_canon)
            continue
        if _empty(v):
            continue
        # Both non-empty.
        if old_v == v:
            if from_canon and not old_canon:
                chosen[canon] = (v, True)
            continue
        # Different values: later occurrence wins.
        chosen[canon] = (v, from_canon)
    return {k: v for k, (v, _) in chosen.items()}


def _local_tool_arg_value_score(value: Any) -> tuple[int, int, int, int]:
    if isinstance(value, dict):
        present = 0
        non_empty = 0
        for v in value.values():
            if v is None:
                continue
            present += 1
            if isinstance(v, str) and not v.strip():
                continue
            if isinstance(v, (list, dict)) and not v:
                continue
            non_empty += 1
        try:
            nbytes = len(json.dumps(value, ensure_ascii=False, separators=(",", ":")))
        except (TypeError, ValueError):
            nbytes = len(str(value))
        return (3, len(value), present, non_empty * 1_000_000 + nbytes)
    if isinstance(value, list):
        try:
            nbytes = len(json.dumps(value, ensure_ascii=False, separators=(",", ":")))
        except (TypeError, ValueError):
            nbytes = len(str(value))
        return (2, len(value), 0, nbytes)
    if value is None:
        return (0, 0, 0, 0)
    text = str(value)
    return (1, 1 if text.strip() else 0, 0, len(text))


def _local_merge_tool_arg_dicts(
    values: list[Any],
    *,
    tool_name: str | None = None,
) -> dict[str, Any] | None:
    dicts = [v for v in values if isinstance(v, dict)]
    if not dicts:
        return None

    def _obj_complete(d: dict[str, Any]) -> bool:
        try:
            text = json.dumps(
                _local_normalize_tool_arg_keys(d),
                ensure_ascii=False,
                separators=(",", ":"),
            )
            return _local_tool_args_ready(text, tool_name=tool_name)
        except (TypeError, ValueError):
            return False

    # Later complete rewrite wins over earlier objects — covers incomplete→complete
    # and complete→complete (wrong early file_path body, later path rewrite).
    base_idx = 0
    last_complete = -1
    for i, d in enumerate(dicts):
        if _obj_complete(d):
            last_complete = i
    if last_complete > 0:
        base_idx = last_complete

    def _is_path_key(key: str) -> bool:
        return _local_canonical_tool_arg_key(str(key or "")) == "file_path"

    def _strip_path_keys(d: dict[str, Any]) -> dict[str, Any]:
        return {k: v for k, v in d.items() if not _is_path_key(k)}

    if base_idx > 0:
        # Later complete rewrite is the base. Earlier path/body must not pin a
        # wrong file_path over the complete payload.
        base = dict(dicts[base_idx])
        base_canons = {_local_canonical_tool_arg_key(str(k)) for k in base.keys()}
        for i, d in enumerate(dicts):
            if i == base_idx:
                continue
            for k, v in d.items():
                canon = _local_canonical_tool_arg_key(str(k))
                if i < base_idx and canon in base_canons:
                    continue
                if k not in base:
                    base[k] = v
                    base_canons.add(canon)
                    continue
                old = base.get(k)
                if old in (None, "", [], {}):
                    base[k] = v
                elif isinstance(v, str) and not v.strip():
                    if i > base_idx:
                        base[k] = v
                elif isinstance(old, dict) and isinstance(v, dict):
                    tmp = dict(old)
                    tmp.update(v)
                    base[k] = tmp
                elif isinstance(old, list) and isinstance(v, list) and len(v) >= len(old):
                    base[k] = v
                elif i > base_idx and v not in (None, "", [], {}):
                    base[k] = v
        return _local_normalize_tool_arg_keys(base)

    # No later-complete base: sequential merge. Incomplete later fragments must
    # not clobber fields from an earlier complete object; non-empty later path
    # still replaces earlier path keys when values differ across aliases.
    first_complete = -1
    for i, d in enumerate(dicts):
        if _obj_complete(d):
            first_complete = i
            break

    merged: dict[str, Any] = {}
    complete_canons: set[str] = set()
    for i, d in enumerate(dicts):
        is_complete = _obj_complete(d)
        if is_complete:
            complete_canons = {
                _local_canonical_tool_arg_key(str(k)) for k in d.keys()
            } | {
                _local_canonical_tool_arg_key(str(k)) for k in merged.keys()
            }
        # If later supplies a non-empty path under any alias, drop earlier path
        # keys so normalize cannot keep a stale file_path over a later path.
        later_path_nonempty = any(
            _is_path_key(k) and v not in (None, "", [], {}) for k, v in d.items()
        )
        if later_path_nonempty and (
            is_complete or first_complete < 0 or i <= first_complete
        ):
            merged = _strip_path_keys(merged)
        for k, v in d.items():
            canon = _local_canonical_tool_arg_key(str(k))
            if (
                first_complete >= 0
                and i > first_complete
                and not is_complete
                and canon in complete_canons
            ):
                # Incomplete later fragment must not clobber complete fields.
                continue
            if k not in merged:
                merged[k] = v
                continue
            old = merged.get(k)
            if old in (None, "", [], {}):
                merged[k] = v
            elif isinstance(v, str) and not v.strip():
                # Later explicit empty string (Edit/Update new_string="" delete).
                merged[k] = v
            elif isinstance(old, dict) and isinstance(v, dict):
                tmp = dict(old)
                tmp.update(v)
                merged[k] = tmp
            elif isinstance(old, list) and isinstance(v, list) and len(v) >= len(old):
                merged[k] = v
            elif v not in (None, "", [], {}):
                merged[k] = v
    return _local_normalize_tool_arg_keys(merged)


def _local_sanitize_tool_arguments_json(
    raw: Any, *, tool_name: str | None = None
) -> str:
    """Import-safe mirror of anthropic_compat.sanitize_tool_arguments_json."""
    if raw is None:
        return ""
    if isinstance(raw, (dict, list)):
        try:
            return json.dumps(raw, ensure_ascii=False, separators=(",", ":"))
        except (TypeError, ValueError):
            return str(raw)
    if not isinstance(raw, str):
        try:
            return json.dumps(raw, ensure_ascii=False, separators=(",", ":"))
        except (TypeError, ValueError):
            return str(raw)
    s = raw
    if not s:
        return ""
    try:
        json.loads(s)
        return s
    except json.JSONDecodeError:
        pass
    stripped = s.strip()
    if stripped and stripped != s:
        try:
            json.loads(stripped)
            return stripped
        except json.JSONDecodeError:
            pass
    decoder = json.JSONDecoder()
    src = stripped or s
    idx = 0
    n = len(src)
    values: list[Any] = []
    ends: list[int] = []
    while idx < n:
        while idx < n and src[idx].isspace():
            idx += 1
        if idx >= n:
            break
        try:
            obj, end = decoder.raw_decode(src, idx)
        except json.JSONDecodeError:
            break
        values.append(obj)
        ends.append(end)
        idx = end
    if len(values) < 2:
        return s
    first = values[0]
    first_text = src[: ends[0]].strip()
    if all(v == first for v in values[1:]):
        return first_text
    merged = _local_merge_tool_arg_dicts(values, tool_name=tool_name)
    candidates: list[tuple[tuple[int, int, int, int], str]] = []
    if merged is not None:
        try:
            merged_text = json.dumps(merged, ensure_ascii=False, separators=(",", ":"))
            candidates.append((_local_tool_arg_value_score(merged), merged_text))
        except (TypeError, ValueError):
            pass
    for i, v in enumerate(values):
        try:
            if i == 0:
                text = first_text
            else:
                start = ends[i - 1]
                while start < ends[i] and src[start].isspace():
                    start += 1
                text = src[start : ends[i]].strip()
                if not text:
                    text = json.dumps(v, ensure_ascii=False, separators=(",", ":"))
            candidates.append((_local_tool_arg_value_score(v), text))
        except (TypeError, ValueError):
            continue
    if not candidates:
        return first_text
    candidates.sort(key=lambda item: item[0], reverse=True)
    return candidates[0][1]


def _local_normalize_tool_arguments_json(
    raw: Any, *, tool_name: str | None = None
) -> str:
    cleaned = _local_sanitize_tool_arguments_json(raw, tool_name=tool_name)
    if not cleaned or not str(cleaned).strip():
        return cleaned
    text = str(cleaned).strip()
    if text[0] not in "{[":
        return cleaned
    try:
        parsed = json.loads(text)
    except Exception:
        return cleaned
    if not isinstance(parsed, dict):
        return cleaned
    normalized = _local_normalize_tool_arg_keys(parsed)
    if normalized == parsed:
        return cleaned
    try:
        return json.dumps(normalized, ensure_ascii=False, separators=(",", ":"))
    except (TypeError, ValueError):
        return cleaned


def _local_tool_args_ready(args: str, *, tool_name: str | None = None) -> bool:
    """Import-safe readiness gate matching anthropic_compat rules.

    Used only when anthropic_compat cannot be imported. Incomplete objects
    (missing required keys) must stay held so we never open response.created
    on a turn that may end empty/malformed for Claude Code.
    """
    text = _local_normalize_tool_arguments_json(args, tool_name=tool_name)
    text = str(text or "").strip()
    if not text or text[0] not in "{[":
        return False
    try:
        parsed = json.loads(text)
    except Exception:
        return False
    if not isinstance(parsed, (dict, list)):
        return False
    if parsed == {} or parsed == []:
        return False
    if isinstance(parsed, dict):
        required = _local_required_keys_for_tool(tool_name)
        # new_string="" is a valid Edit/Update (delete match). Only path-like
        # required keys must be non-empty.
        empty_ok = {"new_string", "new_source", "content"}
        for k in required:
            if k not in parsed:
                return False
            val = parsed.get(k)
            if val is None:
                return False
            if isinstance(val, str) and not val.strip():
                if k not in empty_ok:
                    return False
                continue
            if isinstance(val, (list, dict)) and not val:
                return False
    return True


def _content_parts_to_text(content: Any) -> str:
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if isinstance(content, dict):
        if content.get("text") is not None:
            return str(content.get("text") or "")
        return _stringify(content)
    if not isinstance(content, list):
        return str(content)
    parts: list[str] = []
    for p in content:
        if isinstance(p, str):
            parts.append(p)
            continue
        if not isinstance(p, dict):
            continue
        ptype = str(p.get("type") or "").lower()
        if ptype in ("input_text", "output_text", "text"):
            if p.get("text") is not None:
                parts.append(str(p.get("text") or ""))
        elif p.get("text") is not None:
            parts.append(str(p.get("text") or ""))
    return "".join(parts)


def _multimodal_content_from_parts(parts: list[Any]) -> Any:
    """Return plain string when only text; otherwise OpenAI multimodal list."""
    out: list[dict[str, Any]] = []
    text_only: list[str] = []
    any_non_text = False
    for p in parts:
        if not isinstance(p, dict):
            if isinstance(p, str) and p:
                text_only.append(p)
                out.append({"type": "text", "text": p})
            continue
        ptype = str(p.get("type") or "").lower()
        if ptype in ("input_text", "output_text", "text"):
            t = str(p.get("text") or "")
            text_only.append(t)
            out.append({"type": "text", "text": t})
        elif ptype in ("input_image", "image", "image_url"):
            any_non_text = True
            image_url = p.get("image_url") or p.get("url") or p.get("image")
            if isinstance(image_url, str):
                out.append({"type": "image_url", "image_url": {"url": image_url}})
            elif isinstance(image_url, dict):
                out.append({"type": "image_url", "image_url": image_url})
            else:
                out.append(dict(p))
        else:
            if p.get("text") is not None:
                t = str(p.get("text") or "")
                text_only.append(t)
                out.append({"type": "text", "text": t})
    if not any_non_text:
        return "".join(text_only)
    return out


def convert_responses_input_to_messages(
    raw_input: Any,
    *,
    instructions: str | None = None,
) -> list[dict[str, Any]]:
    """Map Responses `input` (+ optional instructions) → chat messages[]."""
    messages: list[dict[str, Any]] = []
    instr = (instructions or "").strip()
    if instr:
        messages.append({"role": "system", "content": instr})

    if raw_input is None:
        return messages
    if isinstance(raw_input, str):
        text = raw_input.strip()
        if text:
            messages.append({"role": "user", "content": text})
        return messages

    items: list[Any]
    if isinstance(raw_input, dict):
        items = [raw_input]
    elif isinstance(raw_input, list):
        items = raw_input
    else:
        return messages

    for item in items:
        if not isinstance(item, dict):
            if isinstance(item, str) and item.strip():
                messages.append({"role": "user", "content": item})
            continue

        typ = str(item.get("type") or "").lower()
        role = str(item.get("role") or "").lower()

        if typ in ("function_call_output", "tool_result"):
            call_id = (
                str(item.get("call_id") or item.get("tool_call_id") or "").strip()
            )
            output = item.get("output")
            if output is None:
                output = item.get("content")
            messages.append(
                {
                    "role": "tool",
                    "tool_call_id": call_id or f"call_{uuid.uuid4().hex[:12]}",
                    "content": _stringify(output),
                }
            )
            continue

        if typ == "function_call":
            call_id = str(item.get("call_id") or item.get("id") or "").strip()
            name = str(item.get("name") or "").strip()
            args = item.get("arguments")
            if not isinstance(args, str):
                args = _stringify(args)
            tc = {
                "id": call_id or f"call_{uuid.uuid4().hex[:12]}",
                "type": "function",
                "function": {"name": name, "arguments": args or "{}"},
            }
            # Merge consecutive function_call items into one assistant message.
            if (
                messages
                and messages[-1].get("role") == "assistant"
                and messages[-1].get("tool_calls")
                and not (messages[-1].get("content") or "").strip()
            ):
                messages[-1]["tool_calls"].append(tc)
            else:
                messages.append(
                    {"role": "assistant", "content": None, "tool_calls": [tc]}
                )
            continue

        if typ in ("input_text", "text") and not role:
            text = str(item.get("text") or "")
            if text:
                messages.append({"role": "user", "content": text})
            continue

        if typ == "output_text" and not role:
            text = str(item.get("text") or "")
            if text:
                messages.append({"role": "assistant", "content": text})
            continue

        # message item or role-bearing object
        if typ == "message" or role:
            msg_role = role or "user"
            content = item.get("content")
            if isinstance(content, list):
                content = _multimodal_content_from_parts(content)
            elif content is None and item.get("text") is not None:
                content = str(item.get("text") or "")
            elif not isinstance(content, (str, list, dict)) and content is not None:
                content = str(content)

            msg: dict[str, Any] = {"role": msg_role, "content": content}
            # Assistant history may include nested tool calls in rare shapes.
            tcs = item.get("tool_calls")
            if isinstance(tcs, list) and tcs:
                msg["tool_calls"] = tcs
                if msg.get("content") in ("", None):
                    msg["content"] = None
            messages.append(msg)
            continue

    return messages


def convert_responses_tools(tools: Any) -> list[dict[str, Any]] | None:
    """Responses tools (flat function) → chat completions tools[]."""
    if not isinstance(tools, list) or not tools:
        return None
    out: list[dict[str, Any]] = []
    for t in tools:
        if not isinstance(t, dict):
            continue
        ttype = str(t.get("type") or "function").lower()
        if ttype != "function":
            # Built-in search tools are dropped on the chat path.
            continue
        if isinstance(t.get("function"), dict):
            out.append(t)
            continue
        name = t.get("name")
        if not name:
            continue
        fn: dict[str, Any] = {"name": name}
        if t.get("description") is not None:
            fn["description"] = t["description"]
        params = t.get("parameters")
        if params is None:
            params = t.get("input_schema")
        if params is None:
            params = {"type": "object", "properties": {}}
        fn["parameters"] = params
        item: dict[str, Any] = {"type": "function", "function": fn}
        if t.get("strict") is not None:
            # Upstream chat may ignore strict; keep only on function if present.
            try:
                fn["strict"] = bool(t.get("strict"))
            except Exception:
                pass
        out.append(item)
    return out or None


def extract_reasoning_effort(req: dict[str, Any]) -> str | None:
    """Extract and normalize reasoning effort for cli-chat-proxy.

    Upstream grok-4.5 only accepts: low | medium | high | xhigh
    (plus aliases normalized to those). Codex/OpenAI clients often send
    values like auto/default/none/minimal which become Upstream 400
    "Invalid reasoning effort" and empty the account pool.
    """
    raw = req.get("reasoning_effort")
    reasoning = req.get("reasoning")
    if raw is None and isinstance(reasoning, dict):
        raw = reasoning.get("effort")
    if raw is None:
        return None

    # Reuse app normalizer when available (single source of truth).
    try:
        from grok2api.app import _normalize_reasoning_effort  # type: ignore

        return _normalize_reasoning_effort(raw)
    except Exception:
        pass

    if isinstance(raw, bool):
        return "medium" if raw else None
    s = str(raw).strip().lower()
    if not s or s in {"none", "null", "false", "off", "disabled"}:
        return None
    aliases = {
        "minimal": "low",
        "min": "low",
        "l": "low",
        "m": "medium",
        "med": "medium",
        "h": "high",
        "max": "xhigh",
        "maximum": "xhigh",
        "ultra": "xhigh",
        "extra_high": "xhigh",
        "extrahigh": "xhigh",
        "extra-high": "xhigh",
        "enabled": "medium",
        "adaptive": "medium",
        "true": "medium",
        "on": "medium",
        "auto": "medium",
        "default": "medium",
        "standard": "medium",
    }
    s = aliases.get(s, s)
    if s in {"low", "medium", "high", "xhigh"}:
        return s
    # Unknown labels: drop rather than forward (upstream 400s hard).
    return None


def responses_request_to_chat_body(req: dict[str, Any], *, model: str) -> dict[str, Any]:
    """Build an OpenAI chat/completions-shaped body from a Responses request dict."""
    messages = convert_responses_input_to_messages(
        req.get("input"),
        instructions=req.get("instructions")
        if isinstance(req.get("instructions"), str)
        else None,
    )
    body: dict[str, Any] = {
        "model": model,
        "messages": messages,
        "stream": bool(req.get("stream")),
    }

    # Token limits
    if req.get("max_output_tokens") is not None:
        try:
            body["max_tokens"] = int(req["max_output_tokens"])
        except (TypeError, ValueError):
            pass
    elif req.get("max_tokens") is not None:
        try:
            body["max_tokens"] = int(req["max_tokens"])
        except (TypeError, ValueError):
            pass

    tools = convert_responses_tools(req.get("tools"))
    if tools:
        body["tools"] = tools
        # Only forward tool_choice when tools survived conversion. Codex compact
        # and pure-text Responses turns often set tool_choice without tools;
        # upstream rejects that with 400 invalid-argument.
        if req.get("tool_choice") is not None:
            body["tool_choice"] = req.get("tool_choice")
        if req.get("parallel_tool_calls") is not None:
            body["parallel_tool_calls"] = bool(req.get("parallel_tool_calls"))
    if req.get("temperature") is not None:
        body["temperature"] = req.get("temperature")
    if req.get("top_p") is not None:
        body["top_p"] = req.get("top_p")
    if req.get("user") is not None:
        body["user"] = req.get("user")
    effort = extract_reasoning_effort(req)
    if effort:
        body["reasoning_effort"] = effort
    # OpenAI prompt-cache request fields (sub2api / Claude Code / new-api).
    # Kept on the body for sticky affinity; app._sanitize_upstream_body strips
    # them before cli-chat-proxy if unsupported.
    if req.get("prompt_cache_key") is not None:
        body["prompt_cache_key"] = req.get("prompt_cache_key")
    if req.get("prompt_cache_retention") is not None:
        body["prompt_cache_retention"] = req.get("prompt_cache_retention")
    if isinstance(req.get("metadata"), dict):
        # Keep conversation sticky hints if clients put them here.
        meta = req["metadata"]
        if meta.get("user") and not body.get("user"):
            body["user"] = meta.get("user")
        if body.get("prompt_cache_key") in (None, "") and meta.get("prompt_cache_key"):
            body["prompt_cache_key"] = meta.get("prompt_cache_key")
        if body.get("prompt_cache_retention") is None and meta.get(
            "prompt_cache_retention"
        ) is not None:
            body["prompt_cache_retention"] = meta.get("prompt_cache_retention")
    return body


def _detail_cached_tokens(usage: dict[str, Any] | None) -> int:
    if not isinstance(usage, dict):
        return 0
    for parent in ("input_tokens_details", "prompt_tokens_details"):
        node = usage.get(parent)
        if isinstance(node, dict):
            try:
                val = int(node.get("cached_tokens") or 0)
            except (TypeError, ValueError):
                val = 0
            if val > 0:
                return val
    for key in (
        "cached_tokens",
        "cache_read_input_tokens",
        "prompt_cache_hit_tokens",
    ):
        try:
            val = int(usage.get(key) or 0)
        except (TypeError, ValueError):
            val = 0
        if val > 0:
            return val
    return 0


def _detail_cache_creation_tokens(usage: dict[str, Any] | None) -> int:
    if not isinstance(usage, dict):
        return 0
    for key in ("cache_creation_input_tokens", "cache_creation_tokens"):
        try:
            val = int(usage.get(key) or 0)
        except (TypeError, ValueError):
            val = 0
        if val > 0:
            return val
    for parent in ("input_tokens_details", "prompt_tokens_details"):
        node = usage.get(parent)
        if isinstance(node, dict):
            try:
                val = int(node.get("cache_creation_tokens") or 0)
            except (TypeError, ValueError):
                val = 0
            if val > 0:
                return val
    return 0


def _detail_reasoning_tokens(usage: dict[str, Any] | None) -> int:
    if not isinstance(usage, dict):
        return 0
    for parent in ("output_tokens_details", "completion_tokens_details"):
        node = usage.get(parent)
        if isinstance(node, dict):
            try:
                val = int(node.get("reasoning_tokens") or 0)
            except (TypeError, ValueError):
                val = 0
            if val > 0:
                return val
    try:
        return int(usage.get("reasoning_tokens") or 0)
    except (TypeError, ValueError):
        return 0


def chat_usage_to_responses_usage(usage: dict[str, Any] | None) -> dict[str, Any]:
    """Normalize usage for OpenAI Responses clients (sub2api / Claude Code).

    Always includes input_tokens_details.cached_tokens (0 when unknown) so
    secondary relays do not treat a missing key as "no cache support".
    Never invents non-zero cache hits.
    """
    if not isinstance(usage, dict):
        return {
            "input_tokens": 0,
            "output_tokens": 0,
            "total_tokens": 0,
            "input_tokens_details": {"cached_tokens": 0},
            "output_tokens_details": {"reasoning_tokens": 0},
        }
    try:
        inp = int(
            usage.get("prompt_tokens")
            or usage.get("input_tokens")
            or 0
        )
    except (TypeError, ValueError):
        inp = 0
    try:
        out = int(
            usage.get("completion_tokens")
            or usage.get("output_tokens")
            or 0
        )
    except (TypeError, ValueError):
        out = 0
    try:
        total = int(usage.get("total_tokens") or (inp + out))
    except (TypeError, ValueError):
        total = inp + out
    cached = _detail_cached_tokens(usage)
    reasoning = _detail_reasoning_tokens(usage)
    return {
        "input_tokens": inp,
        "output_tokens": out,
        "total_tokens": total,
        "input_tokens_details": {"cached_tokens": cached},
        "output_tokens_details": {"reasoning_tokens": reasoning},
        # Keep chat-completions aliases for mixed clients / ledgers.
        "prompt_tokens": inp,
        "completion_tokens": out,
        "prompt_tokens_details": {"cached_tokens": cached},
        "completion_tokens_details": {"reasoning_tokens": reasoning},
        "cache_read_input_tokens": cached,
        "cache_creation_input_tokens": _detail_cache_creation_tokens(usage),
    }


def build_responses_object(
    *,
    response_id: str,
    model: str,
    content: str,
    reasoning: str = "",
    tool_calls: list[dict[str, Any]] | None = None,
    usage: dict[str, Any] | None = None,
    status: str = "completed",
    created_at: int | None = None,
    previous_response_id: str | None = None,
    metadata: dict[str, Any] | None = None,
    allowed_tool_names: set[str] | list[str] | tuple[str, ...] | None = None,
) -> dict[str, Any]:
    """Assemble a non-stream Responses object from chat completion pieces."""
    output: list[dict[str, Any]] = []
    text = content or ""
    # Optional: expose reasoning as a reasoning item when present (best-effort).
    # sub2api mainly needs message / function_call outputs.
    if text or not tool_calls:
        output.append(
            {
                "id": new_output_item_id("msg"),
                "type": "message",
                "role": "assistant",
                "status": "completed",
                "content": [
                    {
                        "type": "output_text",
                        "text": text,
                    }
                ],
            }
        )
    for tc in tool_calls or []:
        if not isinstance(tc, dict):
            continue
        fn = tc.get("function") if isinstance(tc.get("function"), dict) else {}
        name = (fn.get("name") or tc.get("name") or "").strip()
        if not name:
            continue
        try:
            import grok2api.protocol.anthropic_compat as anth

            name = anth.canonical_outbound_tool_name(
                name, allowed_names=allowed_tool_names
            )
        except Exception:
            pass
        args = fn.get("arguments") if fn else tc.get("arguments")
        if not isinstance(args, str):
            args = _stringify(args) if args is not None else "{}"
        try:
            import grok2api.protocol.anthropic_compat as anth

            args = anth.normalize_tool_arguments_json(args, tool_name=name)
        except Exception:
            pass
        call_id = (tc.get("id") or tc.get("call_id") or "").strip()
        if not call_id:
            call_id = f"call_{uuid.uuid4().hex[:24]}"
        output.append(
            {
                "id": new_output_item_id("fc"),
                "type": "function_call",
                "status": "completed",
                "call_id": call_id,
                "name": name,
                "arguments": args or "{}",
            }
        )

    obj: dict[str, Any] = {
        "id": response_id,
        "object": "response",
        "created_at": int(created_at or time.time()),
        "status": status,
        "model": model,
        "output": output,
        "usage": chat_usage_to_responses_usage(usage),
    }
    if previous_response_id:
        obj["previous_response_id"] = previous_response_id
    if metadata:
        obj["metadata"] = metadata
    # Non-standard debug field is fine; unknown keys are usually ignored.
    if reasoning:
        obj["x_grok2api_reasoning"] = reasoning
    return obj


def sse_event(
    event: str,
    payload: dict[str, Any],
    *,
    sequence_number: int | None = None,
) -> str:
    """Format one Responses SSE frame.

    OpenAI Responses stream events require a monotonic top-level
    ``sequence_number`` (starting at 0). Clients such as the official SDK /
    sub2api fail deserialization with ``missing field sequence_number`` when
    it is absent.
    """
    body = dict(payload or {})
    if "type" not in body and event:
        body["type"] = event
    if sequence_number is not None:
        body["sequence_number"] = int(sequence_number)
    elif "sequence_number" not in body:
        # Defensive default: prefer explicit seq from callers, but never omit.
        body["sequence_number"] = 0
    return f"event: {event}\ndata: {json.dumps(body, ensure_ascii=False)}\n\n"


class _Seq:
    """Tiny monotonic counter for Responses SSE sequence_number."""

    __slots__ = ("n",)

    def __init__(self, start: int = 0) -> None:
        self.n = int(start)

    def next(self) -> int:
        cur = self.n
        self.n += 1
        return cur


def iter_responses_sse_from_completion(
    *,
    response_id: str,
    model: str,
    content: str,
    reasoning: str = "",
    tool_calls: list[dict[str, Any]] | None = None,
    usage: dict[str, Any] | None = None,
    created_at: int | None = None,
    previous_response_id: str | None = None,
    metadata: dict[str, Any] | None = None,
    chunk_chars: int = 48,
    allowed_tool_names: set[str] | list[str] | tuple[str, ...] | None = None,
) -> list[str]:
    """Build a complete Responses SSE sequence from a finished chat completion.

    Emits response.created → (text/tool events) → response.completed.
    Used when we collect upstream first (reliable terminal event for sub2api).
    Every event includes a top-level ``sequence_number`` starting at 0.
    """
    created = int(created_at or time.time())
    frames: list[str] = []
    seq = _Seq(0)

    def emit(event: str, payload: dict[str, Any]) -> None:
        frames.append(sse_event(event, payload, sequence_number=seq.next()))

    initial = {
        "id": response_id,
        "object": "response",
        "created_at": created,
        "status": "in_progress",
        "model": model,
        "output": [],
        "usage": chat_usage_to_responses_usage(None),
    }
    if previous_response_id:
        initial["previous_response_id"] = previous_response_id
    if metadata:
        initial["metadata"] = metadata

    emit("response.created", {"type": "response.created", "response": initial})
    emit(
        "response.in_progress",
        {"type": "response.in_progress", "response": initial},
    )

    output_index = 0
    text = content or ""

    if text:
        msg_id = new_output_item_id("msg")
        emit(
            "response.output_item.added",
            {
                "type": "response.output_item.added",
                "output_index": output_index,
                "item": {
                    "id": msg_id,
                    "type": "message",
                    "role": "assistant",
                    "status": "in_progress",
                    "content": [],
                },
            },
        )
        emit(
            "response.content_part.added",
            {
                "type": "response.content_part.added",
                "item_id": msg_id,
                "output_index": output_index,
                "content_index": 0,
                "part": {"type": "output_text", "text": ""},
            },
        )
        # Emit text in small chunks so clients that only paint on delta still work.
        step = max(8, int(chunk_chars or 48))
        for i in range(0, len(text), step):
            delta = text[i : i + step]
            emit(
                "response.output_text.delta",
                {
                    "type": "response.output_text.delta",
                    "item_id": msg_id,
                    "output_index": output_index,
                    "content_index": 0,
                    "delta": delta,
                },
            )
        emit(
            "response.output_text.done",
            {
                "type": "response.output_text.done",
                "item_id": msg_id,
                "output_index": output_index,
                "content_index": 0,
                "text": text,
            },
        )
        emit(
            "response.content_part.done",
            {
                "type": "response.content_part.done",
                "item_id": msg_id,
                "output_index": output_index,
                "content_index": 0,
                "part": {"type": "output_text", "text": text},
            },
        )
        emit(
            "response.output_item.done",
            {
                "type": "response.output_item.done",
                "output_index": output_index,
                "item": {
                    "id": msg_id,
                    "type": "message",
                    "role": "assistant",
                    "status": "completed",
                    "content": [{"type": "output_text", "text": text}],
                },
            },
        )
        output_index += 1

    for tc in tool_calls or []:
        if not isinstance(tc, dict):
            continue
        fn = tc.get("function") if isinstance(tc.get("function"), dict) else {}
        name = (fn.get("name") or tc.get("name") or "").strip()
        if not name:
            continue
        try:
            import grok2api.protocol.anthropic_compat as anth

            name = anth.canonical_outbound_tool_name(
                name, allowed_names=allowed_tool_names
            )
        except Exception:
            pass
        args = fn.get("arguments") if fn else tc.get("arguments")
        if not isinstance(args, str):
            args = _stringify(args) if args is not None else "{}"
        try:
            import grok2api.protocol.anthropic_compat as anth

            args = anth.normalize_tool_arguments_json(args, tool_name=name)
        except Exception:
            pass
        call_id = (tc.get("id") or tc.get("call_id") or "").strip()
        if not call_id:
            call_id = f"call_{uuid.uuid4().hex[:24]}"
        fc_id = new_output_item_id("fc")
        emit(
            "response.output_item.added",
            {
                "type": "response.output_item.added",
                "output_index": output_index,
                "item": {
                    "id": fc_id,
                    "type": "function_call",
                    "status": "in_progress",
                    "call_id": call_id,
                    "name": name,
                    "arguments": "",
                },
            },
        )
        emit(
            "response.function_call_arguments.delta",
            {
                "type": "response.function_call_arguments.delta",
                "item_id": fc_id,
                "output_index": output_index,
                "delta": args or "{}",
            },
        )
        emit(
            "response.function_call_arguments.done",
            {
                "type": "response.function_call_arguments.done",
                "item_id": fc_id,
                "output_index": output_index,
                "arguments": args or "{}",
            },
        )
        emit(
            "response.output_item.done",
            {
                "type": "response.output_item.done",
                "output_index": output_index,
                "item": {
                    "id": fc_id,
                    "type": "function_call",
                    "status": "completed",
                    "call_id": call_id,
                    "name": name,
                    "arguments": args or "{}",
                },
            },
        )
        output_index += 1

    final = build_responses_object(
        response_id=response_id,
        model=model,
        content=text,
        reasoning=reasoning or "",
        tool_calls=tool_calls,
        usage=usage,
        status="completed",
        created_at=created,
        previous_response_id=previous_response_id,
        metadata=metadata,
        allowed_tool_names=allowed_tool_names,
    )
    emit(
        "response.completed",
        {"type": "response.completed", "response": final},
    )
    # Some clients also accept OpenAI-style done sentinel after events.
    frames.append("data: [DONE]\n\n")
    return frames


def failed_responses_sse(
    *,
    response_id: str,
    message: str,
    err_type: str = "server_error",
    model: str | None = None,
    sequence_number: int | None = None,
    open_envelope: bool = True,
) -> list[str]:
    """Emit a terminal Responses failure stream.

    Bare ``response.failed`` at seq 0 (no prior ``response.created``) and
    ``response.failed`` that rewinds ``sequence_number`` after live deltas both
    make Claude Code / sub2api report
    ``API returned an empty or malformed response (HTTP 200)``.

    Defaults:
    - Never-opened stream (``sequence_number is None`` and ``open_envelope``):
      emit ``response.created`` → ``response.in_progress`` → ``response.failed``
      with monotonic sequence numbers starting at 0.
    - Already-open stream (pass the next ``sequence_number``): emit only
      ``response.failed`` at that number so the sequence never rewinds.
    """
    frames: list[str] = []
    if sequence_number is None:
        seq = _Seq(0)
        if open_envelope:
            initial: dict[str, Any] = {
                "id": response_id,
                "object": "response",
                "created_at": int(time.time()),
                "status": "in_progress",
                "model": model or "",
                "output": [],
                "usage": chat_usage_to_responses_usage(None),
            }
            frames.append(
                sse_event(
                    "response.created",
                    {"type": "response.created", "response": initial},
                    sequence_number=seq.next(),
                )
            )
            frames.append(
                sse_event(
                    "response.in_progress",
                    {"type": "response.in_progress", "response": initial},
                    sequence_number=seq.next(),
                )
            )
        fail_seq = seq.next()
    else:
        fail_seq = int(sequence_number)

    failed_response: dict[str, Any] = {
        "id": response_id,
        "object": "response",
        "status": "failed",
        "error": {"type": err_type, "message": message},
    }
    if model:
        failed_response["model"] = model
    payload = {
        "type": "response.failed",
        "response": failed_response,
    }
    frames.append(
        sse_event("response.failed", payload, sequence_number=fail_seq)
    )
    frames.append("data: [DONE]\n\n")
    return frames


class ResponsesLiveStreamer:
    """Incremental Responses SSE encoder for true first-token streaming.

    Emits response.created immediately, then text/tool events as upstream
    deltas arrive. This avoids the old collect-then-replay path that made
    /v1/responses TTFT equal to full completion latency.
    """

    def __init__(
        self,
        *,
        response_id: str,
        model: str,
        created_at: int | None = None,
        previous_response_id: str | None = None,
        metadata: dict[str, Any] | None = None,
        allowed_tool_names: set[str] | list[str] | tuple[str, ...] | None = None,
    ) -> None:
        self.response_id = response_id
        self.model = model
        self.created_at = int(created_at or time.time())
        self.previous_response_id = previous_response_id
        self.metadata = metadata if isinstance(metadata, dict) else None
        # Client-registered tool names (Claude Code Edit, …). Used to remap
        # model-invented Update/StrReplace → Edit before emission.
        self._allowed_tool_names: set[str] = set(allowed_tool_names or ())
        self._seq = _Seq(0)
        self._started = False
        self._text_open = False
        self._msg_id: str | None = None
        self._text_parts: list[str] = []
        self._tools: dict[int, dict[str, Any]] = {}
        self._tool_opened: set[int] = set()
        self._tool_done: set[int] = set()
        self._output_index = 0
        self._text_output_index = 0
        # _closed means a terminal event (completed/failed + DONE) was shipped.
        # Empty complete() attempts must NOT set this, or fail() becomes a no-op
        # and sub2api reports "stream usage incomplete: missing terminal event".
        self._closed = False

    def _emit(self, event: str, payload: dict[str, Any]) -> str:
        return sse_event(event, payload, sequence_number=self._seq.next())

    def next_sequence_number(self) -> int:
        """Next sequence_number that would be assigned (does not advance)."""
        return int(self._seq.n)

    def fail(self, message: str, *, err_type: str = "server_error") -> list[str]:
        """Terminal failure frames with monotonic sequence_number.

        Opens the envelope first when nothing has been sent yet, so clients
        always see ``response.created`` before ``response.failed``. After live
        deltas, continues the existing counter instead of rewinding to 0 —
        rewinds are reported by Claude Code as empty/malformed HTTP 200.
        """
        if self._closed:
            return []
        frames: list[str] = []
        if not self._started:
            frames.extend(self.start())
        failed_response: dict[str, Any] = {
            "id": self.response_id,
            "object": "response",
            "status": "failed",
            "model": self.model,
            "error": {"type": err_type, "message": message},
        }
        if self.previous_response_id:
            failed_response["previous_response_id"] = self.previous_response_id
        frames.append(
            self._emit(
                "response.failed",
                {"type": "response.failed", "response": failed_response},
            )
        )
        frames.append("data: [DONE]\n\n")
        self._closed = True
        return frames

    def _initial_response(self) -> dict[str, Any]:
        obj: dict[str, Any] = {
            "id": self.response_id,
            "object": "response",
            "created_at": self.created_at,
            "status": "in_progress",
            "model": self.model,
            "output": [],
            "usage": chat_usage_to_responses_usage(None),
        }
        if self.previous_response_id:
            obj["previous_response_id"] = self.previous_response_id
        if self.metadata:
            obj["metadata"] = self.metadata
        return obj

    def start(self) -> list[str]:
        if self._started:
            return []
        self._started = True
        initial = self._initial_response()
        return [
            self._emit(
                "response.created",
                {"type": "response.created", "response": initial},
            ),
            self._emit(
                "response.in_progress",
                {"type": "response.in_progress", "response": initial},
            ),
        ]

    def _ensure_text_open(self) -> list[str]:
        if self._text_open:
            return []
        self._text_open = True
        self._msg_id = new_output_item_id("msg")
        self._text_output_index = self._output_index
        frames = [
            self._emit(
                "response.output_item.added",
                {
                    "type": "response.output_item.added",
                    "output_index": self._text_output_index,
                    "item": {
                        "id": self._msg_id,
                        "type": "message",
                        "role": "assistant",
                        "status": "in_progress",
                        "content": [],
                    },
                },
            ),
            self._emit(
                "response.content_part.added",
                {
                    "type": "response.content_part.added",
                    "item_id": self._msg_id,
                    "output_index": self._text_output_index,
                    "content_index": 0,
                    "part": {"type": "output_text", "text": ""},
                },
            ),
        ]
        self._output_index += 1
        return frames

    def has_client_payload(self) -> bool:
        """True when the client has received real text or a completed tool.

        Envelope-only streams (response.created with no output items) are what
        Claude Code / sub2api report as
        ``API returned an empty or malformed response (HTTP 200)``.
        """
        if any(str(p or "") for p in self._text_parts):
            return True
        if self._tool_done:
            return True
        # Tools opened+args emitted but not yet closed still count as payload.
        for idx, slot in self._tools.items():
            if idx in self._tool_done:
                return True
            if slot.get("args_emitted") and (slot.get("name") or "").strip():
                return True
        return False

    def any_shipable_tool(self, *, terminal: bool = False) -> bool:
        """Public: whether any held tool can ship (strict mid-stream or terminal)."""
        return self._any_shipable_tool(terminal=terminal)

    def held_tool_summary(self) -> list[dict[str, Any]]:
        """Compact debug view of held tool slots (admin / empty-turn diagnostics)."""
        out: list[dict[str, Any]] = []
        for idx in sorted(self._tools.keys()):
            slot = self._tools.get(idx) or {}
            args = slot.get("arguments") or ""
            if not isinstance(args, str):
                args = str(args)
            out.append(
                {
                    "index": idx,
                    "name": (slot.get("name") or "")[:80],
                    "call_id": (slot.get("call_id") or "")[:64],
                    "args_len": len(args),
                    "args_emitted": bool(slot.get("args_emitted")),
                    "done": idx in self._tool_done,
                    "ready_strict": self._tool_is_ready(idx, terminal=False),
                    "ready_terminal": self._tool_is_ready(idx, terminal=True),
                }
            )
        return out

    def on_text_delta(self, delta: str) -> list[str]:
        if not delta or self._closed:
            return []
        frames = self.start()
        frames.extend(self._ensure_text_open())
        self._text_parts.append(delta)
        frames.append(
            self._emit(
                "response.output_text.delta",
                {
                    "type": "response.output_text.delta",
                    "item_id": self._msg_id,
                    "output_index": self._text_output_index,
                    "content_index": 0,
                    "delta": delta,
                },
            )
        )
        return frames

    def _tool_slot(self, index: int) -> dict[str, Any]:
        slot = self._tools.get(index)
        if slot is None:
            slot = {
                "id": new_output_item_id("fc"),
                "call_id": "",
                "name": "",
                "arguments": "",
                "output_index": None,
                "args_emitted": False,
            }
            self._tools[index] = slot
        return slot

    def _merge_tool_name(self, current: str, incoming: str) -> str:
        cur = (current or "").strip()
        inc = (incoming or "").strip()
        if not inc:
            merged = cur
        elif not cur:
            merged = inc
        # True suffix fragment.
        elif inc.startswith(cur):
            merged = inc
        elif cur.endswith(inc):
            merged = cur
        elif inc in cur:
            merged = cur
        else:
            # Divergent full names (Update vs Edit) — prefer the longer / later one
            # then canonicalize below.
            merged = inc if len(inc) >= len(cur) else cur
        try:
            import grok2api.protocol.anthropic_compat as anth

            return anth.canonical_outbound_tool_name(
                merged, allowed_names=self._allowed_tool_names
            )
        except Exception:
            return merged

    def _merge_tool_args(
        self, current: str, incoming: str, *, tool_name: str | None = None
    ) -> str:
        """Merge streamed tool args without double-append corruption.

        Secondary relays often re-send cumulative JSON. Always-append would
        produce `{"file_path":"a"}{"file_path":"a"}` and Claude Code Read fails
        with missing required fields after parse.
        """
        try:
            import grok2api.protocol.anthropic_compat as anth

            return anth.merge_tool_argument_delta(
                current or "", incoming or "", tool_name=tool_name
            )
        except Exception:
            cur = current or ""
            inc = incoming or ""
            if not inc:
                return cur
            if not cur:
                return inc
            if inc.startswith(cur):
                return inc
            if cur.endswith(inc) or inc in cur:
                return cur
            return cur + inc

    def _args_ready(self, args: str, *, tool_name: str | None = None) -> bool:
        """True when tool args are safe to emit mid-stream.

        Prefer anthropic_compat's required-key gate when available. Fall back to a
        local copy of the same rules so a missing pydantic/import error cannot
        open the Responses envelope on syntactically-complete but incomplete
        tool objects (e.g. Read with ``{"path":...}`` instead of ``file_path``).
        Opening early freezes Claude Code / sub2api into an empty/malformed
        HTTP 200 if the turn later yields no client-visible payload.
        """
        try:
            import grok2api.protocol.anthropic_compat as anth

            # Normalize aliases (path/oldString) before readiness.
            norm = anth.normalize_tool_arguments_json(args or "", tool_name=tool_name)
            return bool(
                anth.is_complete_tool_arguments_json(norm or "", tool_name=tool_name)
            )
        except Exception:
            return _local_tool_args_ready(args or "", tool_name=tool_name)

    def _emit_ready_tools(self) -> list[str]:
        """Emit at most one complete tool at a time (name + full JSON args).

        Critical for Claude Code via sub2api:
        - Hold until arguments are complete non-empty JSON object/array.
        - Never stream argument suffixes live (Read.file_path must arrive whole).
        - Never open tool N+1 while tool N is unfinished.
        """
        frames: list[str] = []
        for idx in sorted(self._tools.keys()):
            if idx in self._tool_done:
                continue
            slot = self._tools[idx]
            name = (slot.get("name") or "").strip()
            args = slot.get("arguments") or ""
            # Do not overtake a lower unfinished tool.
            blocked = False
            for lower in range(0, idx):
                if lower in self._tool_done:
                    continue
                low = self._tools.get(lower)
                if not low:
                    continue
                if (low.get("name") or low.get("call_id") or str(low.get("arguments") or "").strip()):
                    blocked = True
                    break
            if blocked:
                break
            if not name:
                # Known id/args without name — keep holding this slot.
                if slot.get("call_id") or str(args).strip():
                    break
                continue
            # Remap Update/StrReplace → Edit (Claude Code registered name).
            try:
                import grok2api.protocol.anthropic_compat as anth

                name = anth.canonical_outbound_tool_name(
                    name, allowed_names=self._allowed_tool_names
                )
                slot["name"] = name
            except Exception:
                pass
            # Normalize aliases before readiness/emission so Update/Edit keys
            # match Claude Code schema even if the model used path/oldString.
            try:
                import grok2api.protocol.anthropic_compat as anth

                args = anth.normalize_tool_arguments_json(args, tool_name=name)
            except Exception:
                args = _local_normalize_tool_arguments_json(args, tool_name=name)
            slot["arguments"] = args
            if not self._args_ready(args, tool_name=name):
                # Hold partial objects (e.g. Update with only file_path).
                break
            # Open + emit full args + close in one burst (atomic for converters).
            if idx not in self._tool_opened:
                if not slot.get("call_id"):
                    slot["call_id"] = f"call_{uuid.uuid4().hex[:24]}"
                slot["output_index"] = self._output_index
                self._output_index += 1
                self._tool_opened.add(idx)
                frames.append(
                    self._emit(
                        "response.output_item.added",
                        {
                            "type": "response.output_item.added",
                            "output_index": slot["output_index"],
                            "item": {
                                "id": slot["id"],
                                "type": "function_call",
                                "status": "in_progress",
                                "call_id": slot["call_id"],
                                "name": name,
                                "arguments": "",
                            },
                        },
                    )
                )
            if not slot.get("args_emitted"):
                frames.append(
                    self._emit(
                        "response.function_call_arguments.delta",
                        {
                            "type": "response.function_call_arguments.delta",
                            "item_id": slot["id"],
                            "output_index": slot["output_index"],
                            "delta": args,
                        },
                    )
                )
                slot["args_emitted"] = True
            frames.append(
                self._emit(
                    "response.function_call_arguments.done",
                    {
                        "type": "response.function_call_arguments.done",
                        "item_id": slot["id"],
                        "output_index": slot["output_index"],
                        "arguments": args,
                    },
                )
            )
            frames.append(
                self._emit(
                    "response.output_item.done",
                    {
                        "type": "response.output_item.done",
                        "output_index": slot["output_index"],
                        "item": {
                            "id": slot["id"],
                            "type": "function_call",
                            "status": "completed",
                            "call_id": slot.get("call_id") or f"call_{uuid.uuid4().hex[:24]}",
                            "name": name,
                            "arguments": args,
                        },
                    },
                )
            )
            self._tool_done.add(idx)
            # One complete tool per on_tool_delta tick — let converters settle.
            break
        return frames

    def _tool_is_ready(self, idx: int, *, terminal: bool = False) -> bool:
        """True when tool slot idx can be shipped (mid-stream or terminal rules)."""
        if idx in self._tool_done:
            return False
        slot = self._tools.get(idx)
        if not slot:
            return False
        name = (slot.get("name") or "").strip()
        if not name:
            return False
        args = slot.get("arguments") or ""
        if terminal:
            return (
                self._terminal_args(
                    args if isinstance(args, str) else str(args), tool_name=name
                )
                is not None
            )
        return self._args_ready(args, tool_name=name)

    def _any_shipable_tool(self, *, terminal: bool = False) -> bool:
        for idx in sorted(self._tools.keys()):
            # Respect ordering: a lower unfinished non-empty slot blocks later ones.
            blocked = False
            for lower in range(0, idx):
                if lower in self._tool_done:
                    continue
                low = self._tools.get(lower)
                if not low:
                    continue
                if (
                    low.get("name")
                    or low.get("call_id")
                    or str(low.get("arguments") or "").strip()
                ):
                    # Lower slot exists; only ship if that lower one is ready too.
                    if not self._tool_is_ready(lower, terminal=terminal):
                        blocked = True
                        break
            if blocked:
                break
            if self._tool_is_ready(idx, terminal=terminal):
                return True
        return False

    def on_tool_delta(self, tool_calls: list[dict[str, Any]] | None) -> list[str]:
        if not tool_calls or self._closed:
            return []
        # Merge slots first WITHOUT opening the Responses envelope. Incomplete
        # tool previews must not emit response.created — that would lock
        # secondary relays into a stream that may end with zero output items
        # (Claude Code: empty/malformed HTTP 200) and block account failover.
        for tc in tool_calls:
            if not isinstance(tc, dict):
                continue
            try:
                idx = int(tc.get("index") if tc.get("index") is not None else 0)
            except (TypeError, ValueError):
                idx = 0
            slot = self._tool_slot(idx)
            if tc.get("id"):
                slot["call_id"] = str(tc.get("id"))
            fn = tc.get("function") if isinstance(tc.get("function"), dict) else {}
            if fn.get("name"):
                slot["name"] = self._merge_tool_name(slot.get("name") or "", str(fn.get("name") or ""))
            if fn.get("arguments") is not None:
                args_piece = fn.get("arguments")
                if not isinstance(args_piece, str):
                    args_piece = _stringify(args_piece)
                slot["arguments"] = self._merge_tool_args(slot.get("arguments") or "", args_piece or "", tool_name=slot.get("name") or "")
            elif tc.get("arguments") is not None:
                args_piece = tc.get("arguments")
                if not isinstance(args_piece, str):
                    args_piece = _stringify(args_piece)
                slot["arguments"] = self._merge_tool_args(slot.get("arguments") or "", args_piece or "", tool_name=slot.get("name") or "")
        # CRITICAL: open envelope BEFORE emitting tool frames so sequence_number
        # stays monotonic (0=response.created, 1=in_progress, then tools…).
        # Emitting tools first assigned seq 0..N to tools and later created got
        # higher numbers — Claude Code / sub2api treat that as malformed HTTP 200.
        if not self._any_shipable_tool(terminal=False):
            return []
        frames = self.start()
        frames.extend(self._emit_ready_tools())
        return frames

    def _close_open_text(self) -> list[str]:
        if not self._text_open or not self._msg_id:
            return []
        text = "".join(self._text_parts)
        frames = [
            self._emit(
                "response.output_text.done",
                {
                    "type": "response.output_text.done",
                    "item_id": self._msg_id,
                    "output_index": self._text_output_index,
                    "content_index": 0,
                    "text": text,
                },
            ),
            self._emit(
                "response.content_part.done",
                {
                    "type": "response.content_part.done",
                    "item_id": self._msg_id,
                    "output_index": self._text_output_index,
                    "content_index": 0,
                    "part": {"type": "output_text", "text": text},
                },
            ),
            self._emit(
                "response.output_item.done",
                {
                    "type": "response.output_item.done",
                    "output_index": self._text_output_index,
                    "item": {
                        "id": self._msg_id,
                        "type": "message",
                        "role": "assistant",
                        "status": "completed",
                        "content": [{"type": "output_text", "text": text}],
                    },
                },
            ),
        ]
        self._text_open = False
        return frames

    def _terminal_args(self, args: str, *, tool_name: str | None = None) -> str | None:
        """Normalize tool args for end-of-stream flush.

        Returns None when args are truncated non-JSON (unsafe to ship).
        Empty args become ``{}``. Complete JSON objects/arrays pass through only
        when they satisfy mid-stream required-key rules for known tools
        (Update/Edit must not ship file_path-only). Unknown tools still flush
        any parseable object so the turn is not empty HTTP 200.
        """
        try:
            import grok2api.protocol.anthropic_compat as anth

            text = anth.normalize_tool_arguments_json(
                args or "", tool_name=tool_name
            )
        except Exception:
            text = _local_normalize_tool_arguments_json(
                args or "", tool_name=tool_name
            )
        text = str(text or "").strip()
        if not text:
            return "{}"
        if text[0] not in "{[":
            return None
        try:
            parsed = json.loads(text)
        except Exception:
            return None
        if not isinstance(parsed, (dict, list)):
            return None
        # Known schema incomplete → refuse terminal flush (prefer failover /
        # empty turn over a wrong Update with only a path).
        try:
            import grok2api.protocol.anthropic_compat as anth

            if not anth.is_complete_tool_arguments_json(text, tool_name=tool_name):
                required = anth._required_keys_for_tool(tool_name)  # type: ignore[attr-defined]
                if required:
                    return None
        except Exception:
            if not _local_tool_args_ready(text, tool_name=tool_name):
                required = _local_required_keys_for_tool(tool_name)
                if required:
                    return None
        return text

    def _close_open_tools(self) -> list[str]:
        """Flush any still-held tools at stream end (best-effort complete JSON)."""
        frames: list[str] = []
        # Prefer the readiness path first (complete JSON only).
        while True:
            more = self._emit_ready_tools()
            if not more:
                break
            frames.extend(more)
        # Terminal flush: emit remaining named tools with parseable JSON args.
        for idx in sorted(self._tools.keys()):
            if idx in self._tool_done:
                continue
            slot = self._tools.get(idx) or {}
            name = (slot.get("name") or "").strip()
            args = slot.get("arguments") or ""
            if not name and not slot.get("call_id") and not str(args).strip():
                continue
            if not name:
                continue
            # Remap Update/StrReplace → Edit on the terminal path too.
            # _emit_ready_tools already remaps, but this fallback used to ship
            # the raw model name when args only became complete at stream end.
            try:
                import grok2api.protocol.anthropic_compat as anth

                name = anth.canonical_outbound_tool_name(
                    name, allowed_names=self._allowed_tool_names
                )
                slot["name"] = name
            except Exception:
                pass
            try:
                import grok2api.protocol.anthropic_compat as anth

                if isinstance(args, str) and args.strip():
                    args = anth.normalize_tool_arguments_json(args, tool_name=name)
                    slot["arguments"] = args
            except Exception:
                pass
            term_args = self._terminal_args(
                args if isinstance(args, str) else str(args), tool_name=name
            )
            if term_args is None:
                # Truncated / schema-incomplete — do not ship garbage Update.
                continue
            args = term_args
            slot["arguments"] = args
            if idx not in self._tool_opened:
                if not slot.get("call_id"):
                    slot["call_id"] = f"call_{uuid.uuid4().hex[:24]}"
                slot["output_index"] = self._output_index
                self._output_index += 1
                self._tool_opened.add(idx)
                frames.append(
                    self._emit(
                        "response.output_item.added",
                        {
                            "type": "response.output_item.added",
                            "output_index": slot["output_index"],
                            "item": {
                                "id": slot["id"],
                                "type": "function_call",
                                "status": "in_progress",
                                "call_id": slot["call_id"],
                                "name": name,
                                "arguments": "",
                            },
                        },
                    )
                )
            out_idx = slot.get("output_index")
            if out_idx is None:
                continue
            if not slot.get("args_emitted"):
                frames.append(
                    self._emit(
                        "response.function_call_arguments.delta",
                        {
                            "type": "response.function_call_arguments.delta",
                            "item_id": slot.get("id"),
                            "output_index": out_idx,
                            "delta": args,
                        },
                    )
                )
                slot["args_emitted"] = True
            frames.append(
                self._emit(
                    "response.function_call_arguments.done",
                    {
                        "type": "response.function_call_arguments.done",
                        "item_id": slot.get("id"),
                        "output_index": out_idx,
                        "arguments": args,
                    },
                )
            )
            frames.append(
                self._emit(
                    "response.output_item.done",
                    {
                        "type": "response.output_item.done",
                        "output_index": out_idx,
                        "item": {
                            "id": slot.get("id"),
                            "type": "function_call",
                            "status": "completed",
                            "call_id": slot.get("call_id") or f"call_{uuid.uuid4().hex[:24]}",
                            "name": name,
                            "arguments": args,
                        },
                    },
                )
            )
            self._tool_done.add(idx)
        return frames

    def _build_completed_response(
        self,
        *,
        usage: dict[str, Any] | None = None,
        reasoning: str = "",
    ) -> dict[str, Any]:
        """Assemble response.completed payload using the SAME item ids as the stream.

        ``build_responses_object`` mints fresh msg_/fc_ ids. Claude Code / sub2api
        correlate stream item ids with the terminal object; mismatched ids look
        like an empty/malformed completed envelope (HTTP 200).
        """
        output: list[dict[str, Any]] = []
        text = "".join(self._text_parts)
        if text:
            output.append(
                {
                    "id": self._msg_id or new_output_item_id("msg"),
                    "type": "message",
                    "role": "assistant",
                    "status": "completed",
                    "content": [{"type": "output_text", "text": text}],
                }
            )

        for idx in sorted(self._tools.keys()):
            if idx not in self._tool_done:
                # Only include tools that were actually shipped to the client.
                continue
            slot = self._tools[idx]
            name = (slot.get("name") or "").strip()
            args = slot.get("arguments") or "{}"
            if not isinstance(args, str):
                args = _stringify(args)
            output.append(
                {
                    "id": slot.get("id") or new_output_item_id("fc"),
                    "type": "function_call",
                    "status": "completed",
                    "call_id": slot.get("call_id") or f"call_{uuid.uuid4().hex[:24]}",
                    "name": name or "",
                    "arguments": args or "{}",
                }
            )

        obj: dict[str, Any] = {
            "id": self.response_id,
            "object": "response",
            "created_at": int(self.created_at),
            "status": "completed",
            "model": self.model,
            "output": output,
            "usage": chat_usage_to_responses_usage(usage),
        }
        if self.previous_response_id:
            obj["previous_response_id"] = self.previous_response_id
        if self.metadata:
            obj["metadata"] = self.metadata
        if reasoning:
            obj["x_grok2api_reasoning"] = reasoning
        return obj

    def complete(
        self,
        *,
        usage: dict[str, Any] | None = None,
        reasoning: str = "",
        force_flush_partial_tools: bool = True,
    ) -> list[str]:
        """Finish the Responses stream.

        Returns ``[]`` when nothing client-visible was produced. Callers must
        treat that as empty upstream (failover / response.failed) instead of
        inventing a completed envelope — Claude Code reports empty completed
        streams as ``API returned an empty or malformed response (HTTP 200)``.

        Mid-stream emission stays strict (required keys). Terminal flush
        (default on) ships named tools whose args are at least valid JSON, so
        a finished turn with real tools is not mistaken for empty upstream.
        Truncated non-JSON args are still dropped.

        Envelope frames are always emitted before tool/text body frames so
        ``sequence_number`` stays monotonic.
        """
        if self._closed:
            return []

        has_text = any(str(p or "") for p in self._text_parts)
        # Already shipped payload?
        already = self.has_client_payload() or has_text
        can_ship_strict = self._any_shipable_tool(terminal=False)
        can_ship_terminal = (
            force_flush_partial_tools and self._any_shipable_tool(terminal=True)
        )
        if not already and not can_ship_strict and not can_ship_terminal:
            # Leave _closed=False so the caller can still emit response.failed
            # via fail(). Closing here made empty turns look like TCP drops to
            # sub2api ("stream usage incomplete: missing terminal event").
            return []

        # Open envelope FIRST (seq 0,1) before any tool/text body events.
        frames = self.start()

        # Prefer strict-ready tools first, then terminal flush of parseable JSON.
        while True:
            more = self._emit_ready_tools()
            if not more:
                break
            frames.extend(more)

        if force_flush_partial_tools:
            frames.extend(self._close_open_tools())

        frames.extend(self._close_open_text())

        if not self.has_client_payload() and not has_text:
            # Safety: should be unreachable after the pre-check, but never emit
            # an empty completed envelope. Keep stream reopenable for fail().
            return []

        final = self._build_completed_response(usage=usage, reasoning=reasoning or "")
        if not final.get("output"):
            # Built envelope but no output items — treat as empty, allow fail().
            return []

        frames.append(
            self._emit(
                "response.completed",
                {"type": "response.completed", "response": final},
            )
        )
        frames.append("data: [DONE]\n\n")
        self._closed = True
        return frames
