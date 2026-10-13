// Runs a Playwright server whose Firefox is displayed on Neko's X display.
import { firefox } from 'playwright-core';

const server = await firefox.launchServer({
  headless: false,
  host: '0.0.0.0',
  port: 3000,
  wsPath: 'pw',
  env: { ...process.env, DISPLAY: process.env.DISPLAY },
});
console.log('playwright server at', server.wsEndpoint());
