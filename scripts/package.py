#!/usr/bin/env python3
"""Cross-compile the Go binary and assemble distributable packages.

Usage: python3 scripts/package.py [os-cpu ...]     (default: all targets)

Outputs (under dist/):
  bin/<os>-<cpu>/        raw binaries (attach these to a GitHub release)
  npm/<os>-<cpu>/        platform packages  (@localimage/<os>-<cpu>)
  npm/localimage/        the main npm package (CLI launcher + SDK)
  wheels/*.whl           one platform wheel per target

Publish with `npm publish` (platform packages first, main package last) and `twine upload`.
See RELEASING.md.
"""
import base64, hashlib, json, os, re, shutil, stat, subprocess, sys, zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DIST = ROOT / "dist"
VERSION = (ROOT / "VERSION").read_text().strip()

# (npm os, npm cpu, GOOS, GOARCH, wheel platform tag)
TARGETS = [
    ("linux", "x64", "linux", "amd64", "manylinux_2_17_x86_64.manylinux2014_x86_64"),
    ("linux", "arm64", "linux", "arm64", "manylinux_2_17_aarch64.manylinux2014_aarch64"),
    ("darwin", "x64", "darwin", "amd64", "macosx_10_9_x86_64"),
    ("darwin", "arm64", "darwin", "arm64", "macosx_11_0_arm64"),
    ("win32", "x64", "windows", "amd64", "win_amd64"),
]
NPM_PLATFORM_NAMES = [f"@localimage/{o}-{c}" for o, c, *_ in TARGETS]

DESCRIPTION = "Run headful browsers in Docker with live view, locally. CLI + SDK."
REPO_URL = "https://github.com/LocalImages/LocalImage"


def die(msg: str) -> None:
    sys.exit(f"package.py: {msg}")


def check_versions() -> None:
    """Every place that carries a version must agree with VERSION."""
    npm = json.loads((ROOT / "packages/npm/package.json").read_text())
    problems = []
    if npm["version"] != VERSION:
        problems.append(f"packages/npm/package.json version is {npm['version']}")
    for name, v in npm.get("optionalDependencies", {}).items():
        if v != VERSION:
            problems.append(f"optionalDependencies {name} is {v}")
    if set(npm.get("optionalDependencies", {})) != set(NPM_PLATFORM_NAMES):
        problems.append("optionalDependencies do not match the TARGETS list")
    m = re.search(r'^version = "([^"]+)"', (ROOT / "packages/python/pyproject.toml").read_text(), re.M)
    if not m or m.group(1) != VERSION:
        problems.append(f"packages/python/pyproject.toml version is {m.group(1) if m else '?'}")
    if problems:
        die(f"version mismatch with VERSION={VERSION}: " + "; ".join(problems)
            + ". Run scripts/bump-version.py.")


def build(goos: str, goarch: str, out: Path) -> None:
    out.parent.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0"}
    subprocess.run(
        ["go", "build", "-trimpath", "-ldflags", f"-s -w -X main.version={VERSION}",
         "-o", str(out), "./cmd/localimage"],
        cwd=ROOT, env=env, check=True)


def npm_platform_package(os_: str, cpu: str, binary: Path, exe: str) -> None:
    d = DIST / "npm" / f"{os_}-{cpu}"
    (d / "bin").mkdir(parents=True, exist_ok=True)
    shutil.copy(binary, d / "bin" / exe)
    (d / "bin" / exe).chmod(0o755)
    shutil.copy(ROOT / "LICENSE", d / "LICENSE")
    (d / "README.md").write_text(
        f"# @localimage/{os_}-{cpu}\n\nPrebuilt `localimage` binary for {os_}-{cpu}. "
        "Install the [`localimage`](https://www.npmjs.com/package/localimage) package instead.\n")
    (d / "package.json").write_text(json.dumps({
        "name": f"@localimage/{os_}-{cpu}",
        "version": VERSION,
        "description": f"localimage binary for {os_}-{cpu}",
        "license": "Apache-2.0",
        "repository": {"type": "git", "url": f"git+{REPO_URL}.git"},
        "homepage": f"{REPO_URL}#readme",
        "os": [os_],
        "cpu": [cpu],
        "files": ["bin", "LICENSE", "README.md"],
        "publishConfig": {"access": "public"},
    }, indent=2) + "\n")


def npm_main_package() -> None:
    src = ROOT / "packages" / "npm"
    d = DIST / "npm" / "localimage"
    d.mkdir(parents=True, exist_ok=True)
    for sub in ("bin", "src"):
        shutil.copytree(src / sub, d / sub)
    (d / "bin" / "localimage.js").chmod(0o755)
    shutil.copy(ROOT / "README.md", d / "README.md")
    shutil.copy(ROOT / "LICENSE", d / "LICENSE")
    pkg = json.loads((src / "package.json").read_text())
    pkg.pop("devDependencies", None)
    pkg.get("scripts", {}).pop("test", None)
    if not pkg.get("scripts"):
        pkg.pop("scripts", None)
    pkg["files"] = ["bin", "src", "LICENSE", "README.md"]
    pkg["publishConfig"] = {"access": "public"}
    (d / "package.json").write_text(json.dumps(pkg, indent=2) + "\n")


def record_line(name: str, data: bytes) -> str:
    h = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
    return f"{name},sha256={h},{len(data)}"


def wheel(plat: str, binary: Path, exe: str) -> Path:
    src = ROOT / "packages" / "python" / "src" / "localimage"
    dist_info = f"localimage-{VERSION}.dist-info"
    readme = (ROOT / "README.md").read_text()
    files: list[tuple[str, bytes, int]] = []  # (arcname, data, mode)
    for py in sorted(src.glob("*.py")):
        files.append((f"localimage/{py.name}", py.read_bytes(), 0o644))
    files.append((f"localimage/bin/{exe}", binary.read_bytes(), 0o755))
    metadata = (
        "Metadata-Version: 2.1\n"
        "Name: localimage\n"
        f"Version: {VERSION}\n"
        f"Summary: {DESCRIPTION}\n"
        "License: Apache-2.0\n"
        "Requires-Python: >=3.9\n"
        f"Project-URL: Homepage, {REPO_URL}\n"
        f"Project-URL: Source, {REPO_URL}\n"
        f"Project-URL: Issues, {REPO_URL}/issues\n"
        "Classifier: License :: OSI Approved :: Apache Software License\n"
        "Classifier: Programming Language :: Python :: 3\n"
        "Classifier: Topic :: Software Development :: Testing\n"
        "Description-Content-Type: text/markdown\n"
        "\n" + readme)
    files.append((f"{dist_info}/METADATA", metadata.encode(), 0o644))
    files.append((f"{dist_info}/LICENSE", (ROOT / "LICENSE").read_bytes(), 0o644))
    files.append((f"{dist_info}/WHEEL", (
        "Wheel-Version: 1.0\nGenerator: localimage-package\n"
        f"Root-Is-Purelib: false\nTag: py3-none-{plat.split('.')[0]}\n"
        + "".join(f"Tag: py3-none-{t}\n" for t in plat.split(".")[1:])).encode(), 0o644))
    files.append((f"{dist_info}/entry_points.txt",
                  b"[console_scripts]\nlocalimage = localimage.cli:main\n", 0o644))
    record = [record_line(n, d) for n, d, _ in files] + [f"{dist_info}/RECORD,,"]
    files.append((f"{dist_info}/RECORD", ("\n".join(record) + "\n").encode(), 0o644))

    out = DIST / "wheels" / f"localimage-{VERSION}-py3-none-{plat}.whl"
    out.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
        for name, data, mode in files:
            zi = zipfile.ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
            zi.external_attr = (stat.S_IFREG | mode) << 16
            zi.compress_type = zipfile.ZIP_DEFLATED
            z.writestr(zi, data)
    return out


def main() -> None:
    check_versions()
    only = set(sys.argv[1:])
    unknown = only - {f"{o}-{c}" for o, c, *_ in TARGETS}
    if unknown:
        die(f"unknown target(s): {', '.join(sorted(unknown))}")
    shutil.rmtree(DIST, ignore_errors=True)
    npm_main_package()
    for os_, cpu, goos, goarch, plat in TARGETS:
        if only and f"{os_}-{cpu}" not in only:
            continue
        exe = "localimage.exe" if goos == "windows" else "localimage"
        binary = DIST / "bin" / f"{os_}-{cpu}" / exe
        print(f"building {os_}-{cpu} ...", flush=True)
        build(goos, goarch, binary)
        npm_platform_package(os_, cpu, binary, exe)
        print("  wheel:", wheel(plat, binary, exe).name)
    print(f"done: version {VERSION}")


if __name__ == "__main__":
    main()
