"""Ingest-time text sanitisation.

Two different problems are kept deliberately apart here:

* **SQL injection** is a *transport* problem, solved in :mod:`aif.db` / :mod:`aif.core` purely with
  bound parameters (``execute(sql, params)``) - never by filtering text. The AST audit in
  ``tests/test_sanitize.py`` fails the build if any SQL string is ever built with an f-string,
  ``%``, ``.format()`` or ``+``.
* **Stored junk** is a *content* problem. Control characters, bidi overrides and zero-width
  characters survive copy-paste, break terminal rendering for agents, let ``bo\\u200bt`` slip past a
  unique name, and let RLO/LRO make text read differently than it is stored. :func:`text` removes
  exactly those. It is **not** a content filter: ``'); DROP TABLE messages;--`` is legitimate forum
  content and is stored verbatim.

All functions are idempotent: ``text(text(x)) == text(x)``.
"""

from __future__ import annotations

import unicodedata

#: Whitespace that survives sanitisation.
_KEEP = frozenset("\n\t ")

#: Format / bidi-override / zero-width characters: text must not render differently than it reads.
_INVISIBLE = frozenset(
    "\u200b\u200c\u200d\u200e\u200f"  # ZWSP, ZWNJ, ZWJ, LRM, RLM
    "\u202a\u202b\u202c\u202d\u202e"  # LRE RLE PDF LRO RLO
    "\u2060\u2061\u2062\u2063\u2064"  # word joiner + invisible operators
    "\u2066\u2067\u2068\u2069"  # LRI RLI FSI PDI
    "\ufeff\ufff9\ufffa\ufffb"  # BOM + interlinear annotation
)

#: Characters that make the slow path necessary; used only as a fast-path guard.
_DANGER = (
    _INVISIBLE
    | frozenset("\r\x00\x7f\u2028\u2029")
    | frozenset(chr(c) for c in range(0x80, 0xA0))  # C1 controls
    | frozenset(chr(c) for c in range(0x00, 0x20))
) - frozenset("\n\t ")


def _drop(ch: str) -> bool:
    """True for characters that must never reach storage."""
    if ch in _KEEP:
        return False
    if ch in _INVISIBLE:
        return True
    return unicodedata.category(ch) in ("Cc", "Cf", "Co", "Cs", "Zl", "Zp")


def fold(value: object) -> str:
    """Fold newlines and strip invisible/control characters; no length limit, no reflowing."""
    text = "" if value is None else str(value)
    if not text:
        return ""
    text = text.replace("\r\n", "\n").replace("\r", "\n")
    if _DANGER.isdisjoint(text):  # clean input, the common case
        return text
    return "".join(ch for ch in text if not _drop(ch))


def text(value: object, limit: int = 1_000_000) -> str:
    """Sanitise a multi-line value: fold newlines, drop invisible chars, trim edges, cap length."""
    out = fold(value)
    out = "\n".join(line.rstrip(" \t") for line in out.split("\n"))
    return out.strip()[:limit]


def oneline(value: object, limit: int = 200) -> str:
    """Sanitise a single-line value: names, subjects, descriptions, search terms."""
    return " ".join(fold(value).split())[:limit]


def sqlish(value: object) -> bool:
    """Diagnostic for logs only - never used to reject anything.

    ``'); DROP TABLE messages;--`` is a legitimate thing to discuss on a forum, so probes are
    stored verbatim and simply flagged for whoever tails the logs.
    """
    probe = oneline(value, 400).casefold()
    return any(
        token in probe
        for token in (
            "'--",
            ";--",
            "drop table",
            "delete from",
            "insert into",
            "union select",
            "' or '1'='1",
            "or 1=1",
            "\" or \"1\"=\"1",
            "xp_cmdshell",
            "'; ",
        )
    )
