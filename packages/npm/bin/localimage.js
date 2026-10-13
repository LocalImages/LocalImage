#!/usr/bin/env node
// Launcher: runs the Go binary from the matching platform package,
// or from LOCALIMAGE_BIN (useful in development).
import { spawnSync } from 'node:child_process';
import { makeExecutable, resolveBinary } from '../src/binary.js';

const bin = resolveBinary();
if (!bin) {
  console.error(`localimage: no binary for ${process.platform}-${process.arch}. Set LOCALIMAGE_BIN to a built binary.`);
  process.exit(1);
}
makeExecutable(bin);

const r = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' });
if (r.error) {
  console.error(`localimage: could not run ${bin}: ${r.error.message}`);
  if (r.error.code === 'EACCES') console.error(`Try: chmod +x "${bin}"`);
  process.exit(1);
}
process.exit(r.status ?? 1);
