#!/usr/bin/env node
// Launcher: runs the Go binary from the matching platform package,
// or from LOCALIMAGE_BIN (useful in development).
import { spawnSync } from 'node:child_process';
import { accessSync, chmodSync, constants } from 'node:fs';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);

function resolveBinary() {
  if (process.env.LOCALIMAGE_BIN) return process.env.LOCALIMAGE_BIN;
  const exe = process.platform === 'win32' ? 'localimage.exe' : 'localimage';
  try {
    return require.resolve(`@localimage/${process.platform}-${process.arch}/bin/${exe}`);
  } catch {
    console.error(`localimage: no binary for ${process.platform}-${process.arch}. Set LOCALIMAGE_BIN to a built binary.`);
    process.exit(1);
  }
}

const bin = resolveBinary();

// Some packaging paths drop the executable bit. Restore it if we are allowed to.
if (process.platform !== 'win32') {
  try {
    accessSync(bin, constants.X_OK);
  } catch {
    try {
      chmodSync(bin, 0o755);
    } catch {
      /* reported below if it still cannot run */
    }
  }
}

const r = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' });
if (r.error) {
  console.error(`localimage: could not run ${bin}: ${r.error.message}`);
  if (r.error.code === 'EACCES') console.error(`Try: chmod +x "${bin}"`);
  process.exit(1);
}
process.exit(r.status ?? 1);
