// Calls ensureDaemon and prints whether this process started the daemon that answered.
// usage: node ensure.mjs <addr>   (LOCALIMAGE_BIN and the daemon env come from the environment)
import { ensureDaemon } from '../../src/index.js';

const r = await ensureDaemon({ addr: process.argv[2], env: { LOCALIMAGE_MAX_SESSIONS: '3' } });
console.log(JSON.stringify({ started: r.started, pid: r.pid ?? null }));
