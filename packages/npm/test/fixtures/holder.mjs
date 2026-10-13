// Process A of a reconnect test: creates a session, attaches, puts state in a page, prints the
// lease (plain JSON) on one line, then drops the connection politely or abruptly.
// usage: node holder.mjs <graceful|abrupt> [chromium|firefox]
import * as pw from 'playwright-core';
import { LocalImage } from '../../src/index.js';

const [mode, browserName = 'chromium'] = process.argv.slice(2);
const li = new LocalImage();
const s = await li.create({ browser: browserName, ttlSeconds: 600 });
const browser = await s.connect(pw);
const ctx = browser.contexts()[0] ?? (await browser.newContext({ viewport: null }));
const page = ctx.pages()[0] ?? (await ctx.newPage());
const html = '<title>state</title><div id=n>0</div><script>setInterval(()=>{n.textContent=+n.textContent+1},100)</script>';
await page.goto('data:text/html,' + encodeURIComponent(html));
await page.evaluate(() => {
  window.marker = 'A-was-here';
});

// The lease is plain data: nothing in it refers to this process.
console.log(JSON.stringify({ id: s.id, browser: browserName, connectUrl: s.connectUrl }));

if (mode === 'graceful') {
  await browser.close(); // for a CDP connection this only disconnects
  process.exit(0);
}
process.exit(3); // abrupt: no close, the connection just goes away
