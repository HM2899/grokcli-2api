# -*- coding: utf-8 -*-
"""Minimal gRPC-web (+proto) codec — exactly enough to reproduce the
accounts.x.ai `auth_mgmt.AuthManagement` calls captured from console.x.ai.

gRPC-web framing (Connect / connect-es 2.1.1, content-type application/grpc-web+proto):

    +--------+----------------+--------------------------+
    | flag   | length (uint32 | payload                  |
    | 1 byte | big-endian)    | (protobuf or trailers)   |
    +--------+----------------+--------------------------+

  * flag 0x00 -> a normal protobuf message frame.
  * flag 0x80 -> a TRAILER frame whose payload is HTTP/1-style headers,
                 e.g. b"grpc-status:0\\r\\n". grpc-status==0 means OK.

A single gRPC-web response body is the concatenation of (optional) message
frame(s) followed by exactly one trailer frame.

Only the protobuf wire types we actually observe are implemented:
  0 = varint, 1 = fixed64, 2 = length-delimited (string/bytes/sub-msg), 5 = fixed32.
"""
from __future__ import annotations

import struct
from typing import Any, Dict, List, Tuple

# protobuf wire types
WT_VARINT = 0
WT_FIXED64 = 1
WT_LEN = 2
WT_FIXED32 = 5


# --------------------------------------------------------------------------- #
# protobuf wire encoding
# --------------------------------------------------------------------------- #
def encode_varint(value: int) -> bytes:
    """Encode a non-negative integer as a base-128 varint."""
    if value < 0:
        raise ValueError("varint must be non-negative")
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def _tag(field_no: int, wire_type: int) -> bytes:
    return encode_varint((field_no << 3) | wire_type)


def encode_string(field_no: int, text: str) -> bytes:
    raw = text.encode("utf-8")
    return _tag(field_no, WT_LEN) + encode_varint(len(raw)) + raw


def encode_bytes(field_no: int, raw: bytes) -> bytes:
    """Encode a length-delimited bytes / nested-message field."""
    return _tag(field_no, WT_LEN) + encode_varint(len(raw)) + raw


def encode_varint_field(field_no: int, value: int) -> bytes:
    return _tag(field_no, WT_VARINT) + encode_varint(value)


def encode_message(fields: List[Tuple[int, str]]) -> bytes:
    """Encode an ordered list of (field_no, string_value) into a protobuf message.

    All AuthManagement requests we reproduce contain only string fields, so this
    is all we need. Field order is preserved to match the captured byte stream.
    """
    out = bytearray()
    for field_no, value in fields:
        out += encode_string(field_no, value)
    return bytes(out)


# --------------------------------------------------------------------------- #
# protobuf wire decoding (generic, best-effort)
# --------------------------------------------------------------------------- #
def _read_varint(data: bytes, i: int) -> Tuple[int, int]:
    result = 0
    shift = 0
    while True:
        b = data[i]
        i += 1
        result |= (b & 0x7F) << shift
        if not (b & 0x80):
            return result, i
        shift += 7


def decode_message(data: bytes) -> List[Dict[str, Any]]:
    """Generic protobuf decoder -> list of {field, type, value/...}.

    Length-delimited fields are surfaced as a UTF-8 string when printable,
    otherwise as hex bytes (likely a nested feedback message).
    """
    fields: List[Dict[str, Any]] = []
    i = 0
    n = len(data)
    while i < n:
        tag, i = _read_varint(data, i)
        field_no = tag >> 3
        wt = tag & 0x07
        if wt == WT_VARINT:
            val, i = _read_varint(data, i)
            fields.append({"field": field_no, "type": "varint", "value": val})
        elif wt == WT_FIXED64:
            chunk = data[i:i + 8]; i += 8
            fields.append({"field": field_no, "type": "fixed64",
                           "double": struct.unpack("<d", chunk)[0] if len(chunk) == 8 else None,
                           "hex": chunk.hex()})
        elif wt == WT_LEN:
            ln, i = _read_varint(data, i)
            chunk = data[i:i + ln]; i += ln
            try:
                s = chunk.decode("utf-8")
                if s.isprintable():
                    fields.append({"field": field_no, "type": "string", "value": s})
                    continue
            except UnicodeDecodeError:
                pass
            fields.append({"field": field_no, "type": "bytes", "hex": chunk.hex(), "len": ln})
        elif wt == WT_FIXED32:
            chunk = data[i:i + 4]; i += 4
            fields.append({"field": field_no, "type": "fixed32",
                           "float": struct.unpack("<f", chunk)[0] if len(chunk) == 4 else None,
                           "uint": struct.unpack("<I", chunk)[0] if len(chunk) == 4 else None,
                           "hex": chunk.hex()})
        else:
            raise ValueError(f"unsupported wire type {wt} at offset {i}")
    return fields


# --------------------------------------------------------------------------- #
# gRPC-web framing
# --------------------------------------------------------------------------- #
def frame_request(message: bytes) -> bytes:
    """Wrap a protobuf message in a single gRPC-web data frame (flag 0x00)."""
    return b"\x00" + struct.pack(">I", len(message)) + message


def parse_response(body: bytes) -> Dict[str, Any]:
    """Split a gRPC-web response body into message frames + trailer.

    Returns {"messages": [decoded_fields, ...], "trailers": {k: v}, "grpc_status": int|None}.
    """
    messages: List[List[Dict[str, Any]]] = []
    trailers: Dict[str, str] = {}
    i = 0
    n = len(body)
    while i + 5 <= n:
        flag = body[i]
        length = struct.unpack(">I", body[i + 1:i + 5])[0]
        payload = body[i + 5:i + 5 + length]
        i += 5 + length
        if flag & 0x80:  # trailer frame
            for line in payload.decode("utf-8", "replace").split("\r\n"):
                if ":" in line:
                    k, v = line.split(":", 1)
                    trailers[k.strip().lower()] = v.strip()
        else:
            messages.append(decode_message(payload))
    grpc_status = int(trailers["grpc-status"]) if "grpc-status" in trailers else None
    return {"messages": messages, "trailers": trailers, "grpc_status": grpc_status}
