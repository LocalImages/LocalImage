// Process B/C of a reconnect test: attaches using nothing but the lease JSON and reports what it
// finds in the page. usage: node attach.mjs '<lease json>'
import * as pw from 'playwright-core';

const lease = JSON.parse(process.argv[2]);
const type = lease.browser === 'firefox' ? pw.firefox : pw.chromium;
const browser = lease.browser === 'firefox' ? await type.connect(lease.connectUrl) : await type.connectOverCDP(lease.connectUrl);
const pages = browser.contexts().flatMap((c) => c.pages());
const page = pages.find((p) => p.url().startsWith('data:text/html'));
if (!page) {
  console.log(JSON.stringify({ ok: false, pages: pages.map((p) => p.url().slice(0, 30)) }));
} else {
  const a = await page.evaluate(() => ({ marker: window.marker, n: +document.getElementById('n').textContent }));
  await page.waitForTimeout(500);
  const n2 = await page.evaluate(() => +document.getElementById('n').textContent);
  console.log(JSON.stringify({ ok: true, marker: a.marker, before: a.n, after: n2 }));
}
await browser.close();
