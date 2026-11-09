#!/usr/bin/env python3
"""Package exactly the five plugin-store members at the ZIP root."""

import json
import pathlib
import sys
import zipfile

PLUGIN_ID = "perplexity"


def library_name(goos: str) -> str:
    """The store looks the library up as <id><ext> for the target GOOS."""
    extension = {"windows": ".dll", "darwin": ".dylib"}.get(goos, ".so")
    return PLUGIN_ID + extension


def members(goos: str):
    return (library_name(goos), "manifest.json", "README.md", "README.zh-CN.md", "LICENSE")


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: package.py <stage> <zip>", file=sys.stderr)
        return 2
    stage = pathlib.Path(sys.argv[1])
    target = pathlib.Path(sys.argv[2])
    parts = target.stem.split("_")
    if len(parts) < 4:
        print(f"unexpected archive name: {target.name}", file=sys.stderr)
        return 2
    goos = parts[-2]
    names = members(goos)
    for name in names:
        if not (stage / name).is_file():
            print(f"missing release member: {name}", file=sys.stderr)
            return 1
    manifest = json.loads((stage / "manifest.json").read_text())
    if manifest["id"] != PLUGIN_ID or manifest["abi_version"] != 1 or manifest["schema_version"] != 10:
        print("invalid Perplexity manifest", file=sys.stderr)
        return 1
    target.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(target, "w", zipfile.ZIP_DEFLATED) as archive:
        for name in names:
            archive.write(stage / name, name)
    print(target)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
