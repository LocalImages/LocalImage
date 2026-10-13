#!/usr/bin/env python3
"""Set the version everywhere: python3 scripts/bump-version.py 0.2.0"""
import json, re, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
if len(sys.argv) != 2 or not re.fullmatch(r"\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?", sys.argv[1]):
    sys.exit("usage: bump-version.py MAJOR.MINOR.PATCH")
v = sys.argv[1]

(ROOT / "VERSION").write_text(v + "\n")

p = ROOT / "packages/npm/package.json"
pkg = json.loads(p.read_text())
pkg["version"] = v
pkg["optionalDependencies"] = {name: v for name in pkg["optionalDependencies"]}
p.write_text(json.dumps(pkg, indent=2) + "\n")

p = ROOT / "packages/python/pyproject.toml"
text, n = re.subn(r'^version = "[^"]+"', f'version = "{v}"', p.read_text(), count=1, flags=re.M)
if n != 1:
    sys.exit("could not find version in pyproject.toml")
p.write_text(text)
print(f"version set to {v}")
