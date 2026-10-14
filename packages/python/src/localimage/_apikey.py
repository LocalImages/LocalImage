"""The daemon generates an API key on first start and keeps it in a file only the current user can
read. The SDK reads the same file, so the same user needs no configuration, while other users and
web pages cannot call the daemon. Mirrors Go's os.UserConfigDir."""
import os
import sys
from typing import Optional


def _config_dir() -> str:
    if os.environ.get("LOCALIMAGE_CONFIG_DIR"):
        return os.environ["LOCALIMAGE_CONFIG_DIR"]
    home = os.path.expanduser("~")
    if sys.platform == "win32":
        return os.environ.get("AppData") or os.path.join(home, "AppData", "Roaming")
    if sys.platform == "darwin":
        return os.path.join(home, "Library", "Application Support")
    return os.environ.get("XDG_CONFIG_HOME") or os.path.join(home, ".config")


def read_key_file() -> Optional[str]:
    """The key from the key file, or None if the daemon has not created one."""
    try:
        with open(os.path.join(_config_dir(), "localimage", "api-key"), encoding="utf-8") as f:
            return f.read().strip() or None
    except OSError:
        return None
