import os
import shutil
import subprocess
import sys


def main() -> None:
    """Run the bundled Go binary (shipped in platform wheels) or LOCALIMAGE_BIN."""
    binary = os.environ.get("LOCALIMAGE_BIN")
    if not binary:
        here = os.path.dirname(__file__)
        exe = "localimage.exe" if sys.platform == "win32" else "localimage"
        bundled = os.path.join(here, "bin", exe)
        binary = bundled if os.path.exists(bundled) else shutil.which("localimage")
    if not binary:
        sys.exit("localimage: no binary found. Set LOCALIMAGE_BIN to a built binary.")
    sys.exit(subprocess.call([binary, *sys.argv[1:]]))
