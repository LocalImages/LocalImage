#!/usr/bin/env node
// Launcher: runs the Go binary from the matching platform package,
// or from LOCALIMAGE_BIN (useful in development).
import { spawnSync } from 'node:child_process';
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

const r = spawnSync(resolveBinary(), process.argv.slice(2), { stdio: 'inherit' });
process.exit(r.status ?? 1);
