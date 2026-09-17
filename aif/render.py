"""Compact text renderers: TSV/JSONL output for list calls (fewer tokens than JSON)."""

from __future__ import annotations

import json
from typing import Any


def compact(value: Any) -> str:
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False)


def _cell(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, bool):
        return "1" if value else "0"
    if isinstance(value, (int, float, str)):
        text = str(value)
    else:
        text = compact(value)
    return text.replace("\t", " ").replace("\r", " ").replace("\n", "\\n")


def to_tsv(payload: Any, section: str = "") -> str:
    """Render a payload as TSV sections: ``#key`` headers, one section per list in *payload*."""
    lines: list[str] = []
    items = [(section, payload)] if isinstance(payload, list) else list(payload.items())
    for key, value in items:
        if isinstance(value, list):
            lines.append(f"#{key}")
            if not value:
                continue
            if isinstance(value[0], dict):
                cols: list[str] = []
                for row in value:
                    for col in row:
                        if col not in cols:
                            cols.append(col)
                lines.append("\t".join(cols))
                lines += ["\t".join(_cell(row.get(col)) for col in cols) for row in value]
            else:
                lines += [_cell(v) for v in value]
        elif isinstance(value, dict):
            lines.append(f"#{key}")
            lines += [f"{k}\t{_cell(v)}" for k, v in value.items()]
        else:
            lines.append(f"{key}\t{_cell(value)}")
    return "\n".join(lines) + "\n"


def to_jsonl(payload: Any, section: str | None = None) -> str:
    """Render the first (or named) list of a payload as one JSON object per line."""
    if isinstance(payload, list):
        rows: list[Any] = payload
    elif section and isinstance(payload.get(section), list):
        rows = payload[section]
    else:
        rows = next((v for v in payload.values() if isinstance(v, list)), [])
    return "\n".join(compact(row) for row in rows) + ("\n" if rows else "")


def render(payload: Any, fmt: str = "json", section: str | None = None) -> tuple[bytes, str]:
    """Return ``(body_bytes, media_type)`` for the requested output format."""
    fmt = (fmt or "json").lower()
    if fmt == "tsv":
        data: Any = payload
        if section is not None and isinstance(payload, dict) and section in payload:
            data = {section: payload[section], **{k: v for k, v in payload.items() if k != section}}
        return to_tsv(data).encode(), "text/tab-separated-values; charset=utf-8"
    if fmt == "jsonl":
        return to_jsonl(payload, section).encode(), "application/x-ndjson; charset=utf-8"
    return compact(payload).encode(), "application/json"
