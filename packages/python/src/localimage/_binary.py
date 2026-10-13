"""Locating the localimage binary. Shared by the CLI shim and the SDK."""
import os
import shutil
import sys
from typing import Optional


def find_binary() -> Optional[str]:
    """Path of the localimage binary: LOCALIMAGE_BIN, the one bundled in the wheel, or PATH."""
    env = os.environ.get("LOCALIMAGE_BIN")
    if env:
        return env
    exe = "localimage.exe" if sys.platform == "win32" else "localimage"
    bundled = os.path.join(os.path.dirname(__file__), "bin", exe)
    return bundled if os.path.exists(bundled) else shutil.which("localimage")


def make_executable(path: str) -> None:
    """Some packaging paths drop the executable bit. Restore it if we are allowed to."""
    if sys.platform == "win32" or os.access(path, os.X_OK):
        return
    try:
        os.chmod(path, 0o755)
    except OSError:
        pass  # the caller reports it if the binary still cannot run
