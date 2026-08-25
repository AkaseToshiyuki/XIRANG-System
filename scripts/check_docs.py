#!/usr/bin/env python3
"""Validate bilingual Markdown coverage and repository-local links."""

from __future__ import annotations

import re
import sys
from pathlib import Path
from urllib.parse import unquote


ROOT = Path(__file__).resolve().parents[1]
IGNORED_PARTS = {".git", "node_modules", "dist", ".data", "public"}
LINK = re.compile(r"(?<!!)\[[^\]]*\]\(([^)]+)\)")


def markdown_files() -> list[Path]:
    return sorted(
        path
        for path in ROOT.rglob("*.md")
        if not any(part in IGNORED_PARTS for part in path.relative_to(ROOT).parts)
    )


def counterpart(path: Path) -> Path:
    if path.name.endswith(".zh-CN.md"):
        return path.with_name(path.name.removesuffix(".zh-CN.md") + ".md")
    return path.with_name(path.stem + ".zh-CN.md")


def local_target(source: Path, raw: str) -> Path | None:
    target = raw.strip().split(maxsplit=1)[0].strip("<>")
    if not target or target.startswith(("#", "http://", "https://", "mailto:")):
        return None
    target = unquote(target.split("#", 1)[0])
    if not target:
        return None
    return (source.parent / target).resolve()


def main() -> int:
    errors: list[str] = []
    files = markdown_files()
    for path in files:
        pair = counterpart(path)
        if not pair.is_file():
            errors.append(
                f"missing bilingual counterpart: {path.relative_to(ROOT)} -> "
                f"{pair.relative_to(ROOT)}"
            )
        text = path.read_text(encoding="utf-8")
        for match in LINK.finditer(text):
            target = local_target(path, match.group(1))
            if target is not None and not target.exists():
                errors.append(
                    f"broken local link: {path.relative_to(ROOT)} -> {match.group(1)}"
                )
    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    print(f"Markdown OK: {len(files)} files, bilingual pairs and local links verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
