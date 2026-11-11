"""Regression checks for prose linting; Chinese text is intentional test data."""

import importlib.util
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("check_docs", Path(__file__).with_name("check-docs.py"))
assert spec is not None and spec.loader is not None
docs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(docs)


class ChinesePunctuationTests(unittest.TestCase):
    def test_reports_original_line_numbers(self):
        text = "# 标题\n\n默认直连,需要时配置代理。\n设置 `proxy_url`; 单独值优先。\n使用全角，保留分号；\n"
        self.assertEqual(docs.chinese_punctuation_lines(text), [3, 4])

    def test_preserves_commands_code_and_urls(self):
        text = """多选用 `1,3`，命令为 `/answer @id 1,3`。
`中文,代码;数据`
``中文,包含 ` 的代码``
```yaml
example: 中文,代码;数据
```
~~~~text
中文,代码
~~~
中文;代码
~~~~
    中文,缩进代码
[说明](https://example.com/中文,路径;参数)
<https://example.com/中文,路径>
https://example.com/中文,路径
[reference]: https://example.com/中文,路径
<!-- 中文,注释
中文;注释 -->
"""
        self.assertEqual(docs.chinese_punctuation_lines(text), [])

    def test_checks_link_labels_and_prose_after_code(self):
        text = "[中文,说明](./file.md)\n`中文,代码`;正文\n[说明](./file.md),请阅读\n[说明](https://example.com/guide),请阅读\n"
        self.assertEqual(docs.chinese_punctuation_lines(text), [1, 2, 3, 4])

    def test_checks_indented_mdx_prose_but_not_attributes(self):
        text = """---
title: 中文,元数据
---
<Card title="中文,属性">
    中文,正文
    ```text
    中文,代码
    ```
</Card>
"""
        self.assertEqual(docs.chinese_punctuation_lines(text, mdx=True), [5])

    def test_multiline_code_keeps_line_numbers(self):
        self.assertEqual(docs.chinese_punctuation_lines("`code\n中文,代码`\n中文;正文"), [3])

    def test_blockquote_fence(self):
        self.assertEqual(docs.chinese_punctuation_lines("> ```text\n> 中文,代码\n> ```\n中文,正文"), [4])


class DocumentationScopeTests(unittest.TestCase):
    def test_current_scope_includes_both_landings_but_no_changelog(self):
        self.assertIn(docs.ROOT / "website/src/content/docs/index.mdx", docs.CURRENT_DOCS)
        self.assertIn(docs.ROOT / "website/src/content/docs/zh/index.mdx", docs.CURRENT_DOCS)
        self.assertNotIn(docs.ROOT / "CHANGELOG_CN.md", docs.CURRENT_DOCS)
        self.assertNotIn(docs.ROOT / "CHANGELOG.md", docs.CURRENT_DOCS)

    def test_retired_claim_is_rejected(self):
        path = docs.ROOT / "docs/configuration_CN.md"
        with patch.object(docs, "CURRENT_DOCS", [path]), patch.object(
            Path, "read_text", return_value="说明\n存在多个 workspace 时必须设置。\n"
        ):
            self.assertEqual(len(docs.check_stale_phrases()), 1)

    def test_binding_requirement_remains_valid(self):
        path = docs.ROOT / "docs/pairing_CN.md"
        with patch.object(docs, "CURRENT_DOCS", [path]), patch.object(
            Path, "read_text", return_value="每个聊天都需显式绑定，单工作区也不例外。\n"
        ):
            self.assertEqual(docs.check_stale_phrases(), [])


class MarkdownAnchorTests(unittest.TestCase):
    def test_renderer_extracts_headings_and_html_ids(self):
        text = """# Title
## Section 1: Intro
### `code_function()` & details
## 多工作区路由与 `/bind`
<a id="custom-anchor"></a>
```markdown
# Not A Real Heading
```
"""
        anchors = docs.render_markdown({Path("sample.md"): text})[Path("sample.md")]["anchors"]
        self.assertIn("title", anchors)
        self.assertIn("section-1-intro", anchors)
        self.assertIn("code_function--details", anchors)
        self.assertIn("多工作区路由与-bind", anchors)
        self.assertIn("custom-anchor", anchors)
        self.assertNotIn("not-a-real-heading", anchors)

    def test_check_local_links_detects_broken_anchors(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            tmp = Path(tmpdir)
            doc_a = tmp / "test_a.md"
            doc_b = tmp / "test_b.md"
            doc_a.write_text("""# Doc A
[Valid same-page](#section-one)
[Invalid same-page](#non-existent)
[Valid cross-page](./test_b.md#section-b)
[Invalid cross-page](./test_b.md#missing-b)
[External URL](https://example.com#external)

## Section One
""", encoding="utf-8")
            doc_b.write_text("""# Doc B
## Section B
""", encoding="utf-8")
            with patch.object(docs, "USER_DOCS", [doc_a]), patch.object(docs, "ROOT", tmp):
                errors = docs.check_local_links()
                self.assertEqual(len(errors), 2)
                self.assertTrue(any("missing anchor #non-existent in test_a.md" in e for e in errors))
                self.assertTrue(any("missing anchor #missing-b in test_b.md" in e for e in errors))

    def test_symlinked_root_reports_relative_paths(self):
        # macOS temp dirs resolve /var to /private/var, so a non-canonical ROOT
        # must not turn error reporting into a ValueError. A symlink makes this
        # regression portable instead of relying on the platform's temp layout.
        with tempfile.TemporaryDirectory() as tmpdir:
            real = Path(tmpdir) / "real"
            real.mkdir()
            doc_a = real / "test_a.md"
            doc_b = real / "test_b.md"
            doc_a.write_text("[Broken](./test_b.md#missing)\n", encoding="utf-8")
            doc_b.write_text("# Doc B\n", encoding="utf-8")
            alias = Path(tmpdir) / "alias"
            try:
                alias.symlink_to(real, target_is_directory=True)
            except (OSError, NotImplementedError) as err:
                self.skipTest(f"symlinks unavailable: {err}")
            with patch.object(docs, "ROOT", alias), patch.object(docs, "USER_DOCS", [alias / "test_a.md"]):
                self.assertEqual(docs.check_local_links(), [
                    "test_a.md:1: missing anchor #missing in test_b.md",
                ])

    def test_target_outside_root_reports_absolute_path(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir) / "repo"
            root.mkdir()
            source = root / "source.md"
            target = Path(tmpdir) / "outside.md"
            source.write_text(
                "[Valid](../outside.md#outside)\n[Broken](../outside.md#missing)\n",
                encoding="utf-8",
            )
            target.write_text("# Outside\n", encoding="utf-8")
            with patch.object(docs, "ROOT", root), patch.object(docs, "USER_DOCS", [source]):
                self.assertEqual(docs.check_local_links(), [
                    f"source.md:2: missing anchor #missing in {target.resolve()}",
                ])

    def test_duplicate_headings_punctuation_and_exact_fragments(self):
        text = """# Page
## Setup
## Setup-1
## Setup
## A & B
## `code_function()`
<a id="Custom-ID"></a>
<a name="Named-ID"></a>
[First](#setup)
[Collision](#setup-1)
[Second](#setup-2)
[Punctuation](#a--b)
[Code](#code_function)
[Explicit](#Custom-ID)
[Named](#Named-ID)
[Encoded](#%43ustom-ID)
[Wrong case](#Setup)
[Wrong spaces](#a-b)
[Wrong explicit case](#custom-id)
"""
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            path = root / "sample.md"
            path.write_text(text, encoding="utf-8")
            with patch.object(docs, "ROOT", root), patch.object(docs, "USER_DOCS", [path]):
                self.assertEqual(docs.check_local_links(), [
                    "sample.md:17: missing anchor #Setup in sample.md",
                    "sample.md:18: missing anchor #a-b in sample.md",
                    "sample.md:19: missing anchor #custom-id in sample.md",
                ])

    def test_code_examples_do_not_create_links_or_anchors(self):
        text = """# Page
````markdown
[Example](./missing.md#example)
```text
# Fake
```
````
~~~~markdown
```
# Also Fake
[Example](#missing)
```
~~~~

    # Indented Fake
    [Example](#missing)

`[Example](#missing)` and ``[Example](./missing.md)``.
<!-- [Example](#missing) -->
[Broken](#fake)
[Also broken](#also-fake)
[Indented broken](#indented-fake)
"""
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            path = root / "sample.md"
            path.write_text(text, encoding="utf-8")
            with patch.object(docs, "ROOT", root), patch.object(docs, "USER_DOCS", [path]):
                self.assertEqual(docs.check_local_links(), [
                    "sample.md:20: missing anchor #fake in sample.md",
                    "sample.md:21: missing anchor #also-fake in sample.md",
                    "sample.md:22: missing anchor #indented-fake in sample.md",
                ])

    def test_raw_html_does_not_change_markdown_heading_numbering(self):
        text = """<div>
<h2>Raw Heading</h2>
</div>

## Raw Heading
[Valid](#raw-heading)
[Broken](#raw-heading-1)
"""
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            path = root / "sample.md"
            path.write_text(text, encoding="utf-8")
            with patch.object(docs, "ROOT", root), patch.object(docs, "USER_DOCS", [path]):
                self.assertEqual(docs.check_local_links(), [
                    "sample.md:7: missing anchor #raw-heading-1 in sample.md",
                ])

    def test_reference_links_html_and_local_images(self):
        text = """# Page
[Valid][ok]
[Broken][bad]
[Image](./image.svg)
![Local image](./missing.svg)
<a href="#Missing">Broken HTML link</a>
<img src="./missing-html.svg">

[ok]: #page
[bad]: #missing
"""
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            path = root / "sample.md"
            path.write_text(text, encoding="utf-8")
            (root / "image.svg").write_text("<svg/>", encoding="utf-8")
            with patch.object(docs, "ROOT", root), patch.object(docs, "USER_DOCS", [path]):
                self.assertEqual(docs.check_local_links(), [
                    "sample.md:3: missing anchor #missing in sample.md",
                    "sample.md:5: missing local link target: ./missing.svg",
                    "sample.md:6: missing anchor #Missing in sample.md",
                    "sample.md:7: missing local link target: ./missing-html.svg",
                ])

    def test_renderer_failure_is_reported_without_skipping_links(self):
        for error in (
            FileNotFoundError("node not found"),
            subprocess.CalledProcessError(1, ["node"], stderr="Cannot find package"),
        ):
            with self.subTest(error=error), patch.object(docs.subprocess, "run", side_effect=error):
                with self.assertRaisesRegex(RuntimeError, "npm ci --prefix website") as caught:
                    docs.render_markdown({Path("sample.md"): "# Page"})
                self.assertIs(caught.exception.__cause__, error)


if __name__ == "__main__":
    unittest.main()
