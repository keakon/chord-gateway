#!/usr/bin/env python3
"""Focused checks for chord-gateway user documentation."""

from __future__ import annotations

import re
import sys
import urllib.parse
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
USER_DOCS = [
    ROOT / "README.md",
    ROOT / "README_CN.md",
    ROOT / "QUICKSTART.md",
    ROOT / "QUICKSTART_CN.md",
    ROOT / "CHANGELOG.md",
    ROOT / "CHANGELOG_CN.md",
    *sorted((ROOT / "docs").glob("*.md")),
]
STALE_PHRASES = [
    "legacy list forms are still accepted",
    "仍接受旧的 list 形式",
    "Application Support/chord-gateway",
    "Inbound Feishu messages must be plain text",
    "飞书入站消息必须是纯文本",
    "plain-text message",
    "纯文本消息",
]

# The always-subscribed control-plane event set is restated in both the runbook
# (operations) and the event-visibility page, in both languages. The copies are
# intentional — the runbook is read standalone — but they drifted silently
# twice (once when the code grew events the docs did not), so they are compared
# here. Everything in this check is inside this repository on purpose: it must
# keep working when only chord-gateway is checked out.
EVENT_LIST_DOCS = [
    ROOT / "docs" / "event-visibility.md",
    ROOT / "docs" / "event-visibility_CN.md",
    ROOT / "docs" / "operations.md",
    ROOT / "docs" / "operations_CN.md",
]
EVENT_LIST_BULLET_RE = re.compile(r"^- `([a-z_]+)`$")
# Anchors identify which bullet list is the event set, so an unrelated bullet
# list in the same page can never be mistaken for it. Only long-lived events
# belong here: a stale list that predates a new event must still be recognized,
# otherwise the failure reads as "list not found" instead of "list is missing
# the new events".
EVENT_LIST_ANCHORS = ("assistant_message", "idle", "error", "agent_done")
EVENT_LIST_MIN_ENTRIES = 8


def check_local_links() -> list[str]:
    errors: list[str] = []
    link_re = re.compile(r"\[[^\]]+\]\(([^)]+)\)")
    for path in USER_DOCS:
        text = path.read_text(encoding="utf-8")
        for match in link_re.finditer(text):
            url = match.group(1).strip()
            if not url or url.startswith("#"):
                continue
            if re.match(r"[a-zA-Z][a-zA-Z0-9+.-]*:", url):
                continue
            target_part = url.split("#", 1)[0]
            if not target_part:
                continue
            target = (path.parent / urllib.parse.unquote(target_part)).resolve()
            if not target.exists():
                line = text[: match.start()].count("\n") + 1
                errors.append(f"{path.relative_to(ROOT)}:{line}: missing local link target: {url}")
    return errors


def check_language_pairs() -> list[str]:
    docs_dir = ROOT / "docs"
    names = sorted(path.name for path in docs_dir.glob("*.md"))
    errors: list[str] = []
    for name in names:
        if name.endswith("_CN.md"):
            english = name.replace("_CN.md", ".md")
            if english not in names:
                errors.append(f"docs/{name}: missing English pair docs/{english}")
        else:
            chinese = name[:-3] + "_CN.md"
            if chinese not in names:
                errors.append(f"docs/{name}: missing Chinese pair docs/{chinese}")
    return errors


def check_stale_phrases() -> list[str]:
    errors: list[str] = []
    # Historical changelog wording can legitimately mention old behavior, so stale
    # phrase checks are limited to current user docs.
    current_docs = [
        ROOT / "README.md",
        ROOT / "README_CN.md",
        ROOT / "QUICKSTART.md",
        ROOT / "QUICKSTART_CN.md",
        *sorted((ROOT / "docs").glob("*.md")),
    ]
    for path in current_docs:
        text = path.read_text(encoding="utf-8")
        for line_no, line in enumerate(text.splitlines(), 1):
            for phrase in STALE_PHRASES:
                if phrase in line:
                    errors.append(f"{path.relative_to(ROOT)}:{line_no}: stale phrase: {phrase}")
    return errors


def _event_list_blocks(text: str) -> list[list[str]]:
    """Return every contiguous run of `- `event`` bullets in one document."""
    blocks: list[list[str]] = []
    current: list[str] = []
    for line in text.splitlines():
        match = EVENT_LIST_BULLET_RE.match(line)
        if match:
            current.append(match.group(1))
            continue
        if current:
            blocks.append(current)
            current = []
    if current:
        blocks.append(current)
    return blocks


def _describe_event_list_difference(reference: list[str], names: list[str]) -> str:
    missing = [name for name in reference if name not in names]
    extra = [name for name in names if name not in reference]
    parts = [f"{len(names)} entries vs {len(reference)}"]
    if missing:
        parts.append("missing " + ", ".join(missing))
    if extra:
        parts.append("unexpected " + ", ".join(extra))
    if not missing and not extra:
        parts.append("same events in a different order")
    return "; ".join(parts)


def check_event_lists() -> list[str]:
    errors: list[str] = []
    found: dict[Path, list[str]] = {}
    for path in EVENT_LIST_DOCS:
        rel = path.relative_to(ROOT)
        if not path.exists():
            errors.append(f"{rel}: missing document that lists the required events")
            continue
        blocks = _event_list_blocks(path.read_text(encoding="utf-8"))
        candidates = [block for block in blocks if all(anchor in block for anchor in EVENT_LIST_ANCHORS)]
        if not candidates:
            errors.append(
                f"{rel}: no bullet list contains the anchor events ({', '.join(EVENT_LIST_ANCHORS)}); "
                "expected the always-subscribed event set"
            )
            continue
        names = max(candidates, key=len)
        if len(names) < EVENT_LIST_MIN_ENTRIES:
            errors.append(f"{rel}: event list has only {len(names)} entries; expected at least {EVENT_LIST_MIN_ENTRIES}")
            continue
        found[path] = names

    if len(found) < 2:
        return errors
    reference_path, reference = next(iter(found.items()))
    for path, names in found.items():
        if names == reference:
            continue
        difference = _describe_event_list_difference(reference, names)
        errors.append(
            f"{path.relative_to(ROOT)}: event list differs from {reference_path.relative_to(ROOT)} ({difference})"
        )
    return errors


def main() -> int:
    checks = [
        ("local links", check_local_links()),
        ("language pairs", check_language_pairs()),
        ("stale phrases", check_stale_phrases()),
        ("event lists", check_event_lists()),
    ]
    failed = False
    for name, errors in checks:
        if errors:
            failed = True
            print(f"FAIL: {name}")
            for error in errors:
                print(f"  {error}")
        else:
            print(f"OK: {name}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
