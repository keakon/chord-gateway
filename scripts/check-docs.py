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


def main() -> int:
    checks = [
        ("local links", check_local_links()),
        ("language pairs", check_language_pairs()),
        ("stale phrases", check_stale_phrases()),
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
