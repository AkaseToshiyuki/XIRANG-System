#!/usr/bin/env python3
"""Collect exact dependency license files into a native release tree."""

from __future__ import annotations

import json
import pathlib
import shutil
import subprocess
import sys


ROOT = pathlib.Path(__file__).resolve().parents[1]
LICENSE_NAMES = ("LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING", "NOTICE", "NOTICE.txt")
WEB_ROOTS = ("@fontsource-variable/noto-sans-sc", "react", "react-dom")


def go_modules() -> list[dict[str, str]]:
    output = subprocess.check_output(
        ["go", "list", "-deps", "-json", "./cmd/..."], cwd=ROOT, text=True
    )
    decoder = json.JSONDecoder()
    cursor = 0
    modules: dict[tuple[str, str], dict[str, str]] = {}
    while cursor < len(output):
        while cursor < len(output) and output[cursor].isspace():
            cursor += 1
        if cursor >= len(output):
            break
        package, cursor = decoder.raw_decode(output, cursor)
        module = package.get("Module")
        if module and module.get("Path") and module.get("Dir") and not module.get("Main"):
            modules[(module["Path"], module.get("Version", "unknown"))] = module
    return [modules[key] for key in sorted(modules)]


def web_modules() -> list[dict[str, str]]:
    pending = list(WEB_ROOTS)
    seen: set[str] = set()
    result: list[dict[str, str]] = []
    while pending:
        name = pending.pop(0)
        if name in seen:
            continue
        seen.add(name)
        package_file = ROOT / "web" / "node_modules" / name / "package.json"
        if not package_file.is_file():
            raise SystemExit(f"missing Web dependency; run npm ci first: {name}")
        package = json.loads(package_file.read_text(encoding="utf-8"))
        result.append({"Path": name, "Version": package.get("version", "unknown"), "Dir": str(package_file.parent)})
        pending.extend(sorted((package.get("dependencies") or {}).keys()))
    return sorted(result, key=lambda item: item["Path"])


def safe_name(value: str) -> str:
    return value.replace("/", "__").replace("@", "_")


def collect(component: dict[str, str], family: str, target: pathlib.Path) -> dict[str, object]:
    source = pathlib.Path(component["Dir"])
    destination = target / family / f"{safe_name(component['Path'])}@{component['Version']}"
    copied: list[str] = []
    for name in LICENSE_NAMES:
        candidate = source / name
        if candidate.is_file():
            destination.mkdir(parents=True, exist_ok=True)
            shutil.copy2(candidate, destination / name)
            copied.append(name)
    if not copied:
        raise SystemExit(f"dependency has no recognized license file: {component['Path']}")
    return {"name": component["Path"], "version": component["Version"], "files": copied}


def main() -> int:
    if len(sys.argv) != 2:
        raise SystemExit("usage: collect_third_party_licenses.py OUTPUT_DIRECTORY")
    target = pathlib.Path(sys.argv[1]).resolve()
    if target == pathlib.Path("/"):
        raise SystemExit("refusing to replace filesystem root")
    shutil.rmtree(target, ignore_errors=True)
    target.mkdir(parents=True)
    manifest = {
        "go": [collect(module, "go", target) for module in go_modules()],
        "web": [collect(module, "web", target) for module in web_modules()],
    }
    (target / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
