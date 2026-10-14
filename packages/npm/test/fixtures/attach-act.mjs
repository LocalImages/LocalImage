// A second process acts on an existing session knowing only its id: uploads a file, records a
// few seconds, reads the logs. usage: node attach-act.mjs <session id>
import { LocalImage } from '../../src/index.js';

const li = new LocalImage();
const s = await li.attach(process.argv[2]);
await s.uploadFile(`${s.downloadsDir}/from-other-process.txt`, 'written by another process');
const started = await s.startRecording();
await new Promise((r) => setTimeout(r, 2000));
const stopped = await s.stopRecording();
const log = await s.logs({ source: 'neko', tail: 5 });
console.log(JSON.stringify({ id: s.id, replay_id: started.replay_id, stopped: stopped.status, logLines: log.trim().split('\n').length }));
