import subprocess
import sys

from ._binary import find_binary, make_executable


def main() -> None:
    """Run the bundled Go binary (shipped in platform wheels) or LOCALIMAGE_BIN."""
    binary = find_binary()
    if not binary:
        sys.exit("localimage: no binary found. Set LOCALIMAGE_BIN to a built binary.")
    make_executable(binary)
    try:
        sys.exit(subprocess.call([binary, *sys.argv[1:]]))
    except OSError as e:
        sys.exit(f"localimage: could not run {binary}: {e}")
