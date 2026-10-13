// Locating the localimage binary. Shared by the CLI launcher and the SDK.
import { accessSync, chmodSync, constants } from 'node:fs';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);

/** Path of the localimage binary, or null if there is none for this platform. */
export function resolveBinary() {
  if (process.env.LOCALIMAGE_BIN) return process.env.LOCALIMAGE_BIN;
  const exe = process.platform === 'win32' ? 'localimage.exe' : 'localimage';
  try {
    return require.resolve(`@localimage/${process.platform}-${process.arch}/bin/${exe}`);
  } catch {
    return null;
  }
}

/** Some packaging paths drop the executable bit. Restore it if we are allowed to. */
export function makeExecutable(bin) {
  if (process.platform === 'win32') return;
  try {
    accessSync(bin, constants.X_OK);
  } catch {
    try {
      chmodSync(bin, 0o755);
    } catch {
      /* the caller reports it if the binary still cannot run */
    }
  }
}
