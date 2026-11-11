#!/usr/bin/env python3
"""Focused checks for chord-gateway user documentation."""

from __future__ import annotations

import json
import re
import subprocess
import sys
import urllib.parse
from html.parser import HTMLParser
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MARKDOWN_RENDERER = ROOT / "website/scripts/render-doc-links.mjs"
USER_DOCS = [
    ROOT / "README.md",
    ROOT / "README_CN.md",
    ROOT / "QUICKSTART.md",
    ROOT / "QUICKSTART_CN.md",
    ROOT / "CHANGELOG.md",
    ROOT / "CHANGELOG_CN.md",
    *sorted((ROOT / "docs").glob("*.md")),
]
CURRENT_DOCS = [
    path for path in USER_DOCS if path.name not in {"CHANGELOG.md", "CHANGELOG_CN.md"}
] + [
    ROOT / "website/src/content/docs/index.mdx",
    ROOT / "website/src/content/docs/zh/index.mdx",
]
STALE_PHRASES = [
    "legacy list forms are still accepted",
    "仍接受旧的 list 形式",
    "Application Support/chord-gateway",
    "Inbound Feishu messages must be plain text",
    "飞书入站消息必须是纯文本",
    "plain-text message",
    "纯文本消息",
    "微信始终路由到一个 workspace。",
    "存在多个 workspace 时必须设置。",
    "For Feishu multi-workspace mode, configure",
    "飞书多工作区模式下，请配置",
    # Reject the retired all-platform introduction, not the binding requirement.
    "All IM platforms use one paired owner and explicit chat bindings, including single-workspace deployments. Run",
    "所有 IM 平台统一使用单 owner 配对与显式聊天绑定，单工作区也需绑定。首次运行",
]
CHINESE_PUNCTUATION_RE = re.compile(r"(?<=[\u3400-\u9fff])[,;]|[,;](?=[ \t]*[\u3400-\u9fff])")

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

# Numeric constants that are restated in user-facing docs. Doc/code drift here
# was caught only by manual review, so each explicit pair below is compared on
# every run. Numbers are normalized by stripping commas and spaces, so a doc's
# "100,000" matches a source 100000. To extend the table, add an entry with the
# Go file and regex that extract the value, plus one (doc, regex) pair for each
# place that must state it. Anchor the doc regex on stable wording next to the
# number so it cannot latch onto an unrelated value; if the sentence is
# reworded, update the regex (or drop the pair when the doc stops making the
# claim).
DOCUMENTED_CONSTANTS = [
    {
        "name": "ReplayCapacity",
        "source": "internal/discordgateway/gateway.go",
        "source_re": r"\bconst ReplayCapacity = (\d+)",
        "docs": [
            ("docs/operations.md", r"Resume buffers at most ([\d,]+) useful events"),
            ("docs/discord.md", r"replay buffer holds at most ([\d,]+) useful events"),
        ],
    },
    {
        "name": "ingressHistoryLimit",
        "source": "internal/gateway/im_ingress_store.go",
        "source_re": r"\bconst ingressHistoryLimit = (\d+)",
        "docs": [
            ("docs/operations.md", r"share a ([\d,]+)-record"),
            ("docs/operations_CN.md", r"另受 ([\d,]+) 条"),
        ],
    },
]


class HTMLAnchors(HTMLParser):
    def __init__(self, html: str):
        super().__init__()
        self.anchors: set[str] = set()
        self.feed(html)

    def handle_starttag(self, tag, attrs):
        for name, value in attrs:
            if value is not None and (name == "id" or (tag == "a" and name == "name")):
                self.anchors.add(value)


def render_markdown(documents: dict[Path, str]) -> dict:
    # Render in one process so all links and IDs use the website's locked parser.
    try:
        result = subprocess.run(
            ["node", str(MARKDOWN_RENDERER)],
            input=json.dumps({str(path): text for path, text in documents.items()}),
            capture_output=True, text=True, check=True,
        )
    except (OSError, subprocess.CalledProcessError) as err:
        detail = err.stderr if isinstance(err, subprocess.CalledProcessError) else str(err)
        raise RuntimeError(
            "Markdown rendering failed. Use Node >=22.12.0 and run "
            f"`npm ci --prefix website` before docs checks.\n{detail}"
        ) from err
    return {
        Path(name): {"anchors": HTMLAnchors(page["html"]).anchors, "links": page["links"]}
        for name, page in json.loads(result.stdout).items()
    }


def _display_path(path: Path, root: Path) -> str:
    """Format a doc path relative to ROOT, even when symlinks make prefixes differ.

    Link targets are resolved before they are looked up, while ROOT and USER_DOCS
    may be non-canonical (symlinked checkouts, temporary roots in tests). Resolve
    both sides before comparing; targets outside ROOT keep their absolute path
    rather than turning a lint error into a ValueError.
    """
    try:
        return str(path.resolve().relative_to(root))
    except ValueError:
        return str(path)


def check_local_links() -> list[str]:
    errors: list[str] = []
    root = ROOT.resolve()
    pages = render_markdown({path: path.read_text(encoding="utf-8") for path in USER_DOCS})
    for path in USER_DOCS:
        display_path = _display_path(path, root)
        for link in pages[path]["links"]:
            url = urllib.parse.urlsplit(link["url"])
            if url.scheme or url.netloc:
                continue
            target = (path.parent / urllib.parse.unquote(url.path)).resolve() if url.path else path
            fragment = urllib.parse.unquote(url.fragment)
            location = f"{display_path}:{link['line']}"
            if not target.exists():
                errors.append(f"{location}: missing local link target: {link['url']}")
            elif fragment and target.suffix in {".md", ".mdx"}:
                if target not in pages:
                    pages.update(render_markdown({target: target.read_text(encoding="utf-8")}))
                if fragment not in pages[target]["anchors"]:
                    errors.append(
                        f"{location}: missing anchor #{fragment} in {_display_path(target, root)}"
                    )
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
    for path in CURRENT_DOCS:
        text = path.read_text(encoding="utf-8")
        for line_no, line in enumerate(text.splitlines(), 1):
            for phrase in STALE_PHRASES:
                if phrase in line:
                    errors.append(f"{path.relative_to(ROOT)}:{line_no}: stale phrase: {phrase}")
    return errors


def chinese_punctuation_lines(text: str, *, mdx: bool = False) -> list[int]:
    """Find ASCII commas/semicolons next to Chinese prose, preserving line numbers."""
    lines: list[str] = []
    fence = ""
    frontmatter = False
    for number, line in enumerate(text.splitlines(), 1):
        if number == 1 and line == "---":
            frontmatter = True
            lines.append("")
            continue
        if frontmatter:
            if line == "---":
                frontmatter = False
            lines.append("")
            continue
        marker = re.match(r"^[ \t]*(?:>[ \t]*)?(`{3,}|~{3,})(.*)$", line)
        if fence:
            if marker and marker[1][0] == fence[0] and len(marker[1]) >= len(fence) and not marker[2].strip():
                fence = ""
            lines.append("")
            continue
        if marker:
            fence = marker[1]
            lines.append("")
            continue
        if not mdx and (line.startswith("    ") or line.startswith("\t")):
            lines.append("")
            continue
        lines.append(line)

    prose = "\n".join(lines)

    def mask(match: re.Match[str]) -> str:
        return re.sub(r"[^\n]", " ", match[0])

    # Keep link labels visible, but exclude syntax/data and HTML/MDX attributes.
    for pattern in (
        r"<!--[\s\S]*?-->",
        r"(?<!`)(`+)(?!`)[\s\S]*?(?<!`)\1(?!`)",
    ):
        prose = re.sub(pattern, mask, prose)
    prose = re.sub(
        r"(!?\[[^\]]*\])\([^\n]*?\)",
        lambda m: m[1] + " " * (len(m[0]) - len(m[1])),
        prose,
    )
    for pattern in (
        r"(?m)^[ \t]*\[[^\]]+\]:[^\n]*",
        r"<[^>\n]+>",
        r"https?://[^\s<>]+",
    ):
        prose = re.sub(pattern, mask, prose)
    return [
        number for number, line in enumerate(prose.splitlines(), 1)
        if CHINESE_PUNCTUATION_RE.search(line)
    ]


def check_chinese_punctuation() -> list[str]:
    errors: list[str] = []
    for path in CURRENT_DOCS:
        if not (path.name.endswith("_CN.md") or path.parent.name == "zh"):
            continue
        for line in chinese_punctuation_lines(path.read_text(encoding="utf-8"), mdx=path.suffix == ".mdx"):
            errors.append(f"{path.relative_to(ROOT)}:{line}: ASCII comma/semicolon next to Chinese prose")
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


def _normalize_number(value: str) -> str:
    return re.sub(r"[\s,]", "", value)


def check_documented_constants() -> list[str]:
    errors: list[str] = []
    for entry in DOCUMENTED_CONSTANTS:
        source = ROOT / entry["source"]
        if not source.exists():
            errors.append(f"{entry['source']}: missing file that defines {entry['name']}")
            continue
        source_match = re.search(entry["source_re"], source.read_text(encoding="utf-8"))
        if not source_match:
            errors.append(f"{entry['source']}: cannot find {entry['name']} with pattern {entry['source_re']!r}")
            continue
        expected = _normalize_number(source_match.group(1))
        for doc_rel, doc_re in entry["docs"]:
            doc_path = ROOT / doc_rel
            if not doc_path.exists():
                errors.append(f"{doc_rel}: missing document that states {entry['name']}")
                continue
            doc_match = re.search(doc_re, doc_path.read_text(encoding="utf-8"))
            if not doc_match:
                errors.append(
                    f"{doc_rel}: no text matches {doc_re!r}; restate {entry['name']} or update the table"
                )
                continue
            documented = _normalize_number(doc_match.group(1))
            if documented != expected:
                errors.append(
                    f"{doc_rel}: documents {entry['name']} as {doc_match.group(1)} "
                    f"but the source has {source_match.group(1)}"
                )
    return errors


def main() -> int:
    checks = [
        ("local links", check_local_links()),
        ("language pairs", check_language_pairs()),
        ("stale phrases", check_stale_phrases()),
        ("Chinese prose punctuation", check_chinese_punctuation()),
        ("event lists", check_event_lists()),
        ("documented constants", check_documented_constants()),
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
    try:
        sys.exit(main())
    except RuntimeError as err:
        print(err, file=sys.stderr)
        sys.exit(1)
