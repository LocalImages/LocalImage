import { readFileSync } from 'node:fs';
import { homedir, platform } from 'node:os';
import { join } from 'node:path';

// The daemon generates an API key on first start and keeps it in a file only the current user can
// read. The SDK reads the same file, so the same user needs no configuration, while other users and
// web pages cannot call the daemon. This mirrors Go's os.UserConfigDir.
function configDir() {
  if (platform() === 'win32') return process.env.AppData ?? join(homedir(), 'AppData', 'Roaming');
  if (platform() === 'darwin') return join(homedir(), 'Library', 'Application Support');
  return process.env.XDG_CONFIG_HOME || join(homedir(), '.config');
}

/** The key from the key file, or undefined if the daemon has not created one. */
export function readKeyFile() {
  try {
    return readFileSync(join(configDir(), 'localimage', 'api-key'), 'utf8').trim() || undefined;
  } catch {
    return undefined;
  }
}
