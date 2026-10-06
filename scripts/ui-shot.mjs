#!/usr/bin/env node
// ui-shot.mjs — render a page in headless Chrome via the DevTools protocol,
// report console errors, run an expression, save a screenshot.
//
//   node scripts/ui-shot.mjs URL OUT.png [EXPR] [CLICK_SELECTOR]
//
// Used to smoke-test the embedded web UI without a browser on screen. Needs
// Node >= 22 (global fetch + WebSocket) and google-chrome on PATH. Exit 3 when
// the page logged errors or threw. Pass ?nolive in the URL so the page does
// not hold the event stream open.
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const [url, out, expr = 'document.querySelectorAll("#list tbody tr").length', click = ''] = process.argv.slice(2);
if (!url || !out) { console.error('usage: ui-shot.mjs URL OUT.png [EXPR] [CLICK_SELECTOR]'); process.exit(2); }

const port = 9222 + Math.floor(Math.random() * 500);
const prof = mkdtempSync(join(tmpdir(), 'ui-shot-'));
const chrome = spawn('google-chrome', ['--headless=new', '--disable-gpu', '--no-sandbox', '--hide-scrollbars',
  `--user-data-dir=${prof}`, `--remote-debugging-port=${port}`, '--window-size=1700,1100', 'about:blank'], { stdio: 'ignore' });
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
const fail = (m) => { console.error('ui-shot:', m); cleanup(); process.exit(1); };
function cleanup() { try { chrome.kill('SIGKILL'); } catch { } try { rmSync(prof, { recursive: true, force: true }); } catch { } }
setTimeout(() => fail('timeout'), 40000).unref();

let target;
for (let i = 0; i < 50 && !target; i++) {
  try { const list = await (await fetch(`http://127.0.0.1:${port}/json`)).json(); target = list.find(t => t.type === 'page'); } catch { await sleep(200); }
}
if (!target) fail('chrome did not expose a page target');
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
let id = 0; const pending = new Map(); const errors = [];
ws.onmessage = (m) => {
  const msg = JSON.parse(m.data);
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
  if (msg.method === 'Runtime.exceptionThrown') errors.push('exception: ' + (msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text));
  if (msg.method === 'Runtime.consoleAPICalled' && (msg.params.type === 'error' || msg.params.type === 'warning')) errors.push(msg.params.type + ': ' + msg.params.args.map(a => a.value ?? a.description).join(' '));
  if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') errors.push('log: ' + msg.params.entry.text + ' ' + (msg.params.entry.url || ''));
};
const send = (method, params = {}) => new Promise(res => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params })); });

await send('Runtime.enable'); await send('Log.enable'); await send('Page.enable');
await send('Emulation.setDeviceMetricsOverride', { width: 1700, height: 1100, deviceScaleFactor: 1, mobile: false });
await send('Page.navigate', { url });
await sleep(2500);
if (click) {
  await send('Runtime.evaluate', { expression: `(() => { const el = document.querySelector(${JSON.stringify(click)}); if (!el) throw new Error('no element ' + ${JSON.stringify(click)}); el.click(); return true; })()`, awaitPromise: true });
  await sleep(2500);
}
const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
if (r.error) errors.push('evaluate: ' + JSON.stringify(r.error));
if (r.result?.exceptionDetails) errors.push('expression threw: ' + (r.result.exceptionDetails.exception?.description || r.result.exceptionDetails.text));
const shot = await send('Page.captureScreenshot', { format: 'png' });
if (shot.result?.data) writeFileSync(out, Buffer.from(shot.result.data, 'base64'));
else errors.push('screenshot: ' + JSON.stringify(shot.error || shot));
console.log(JSON.stringify({ result: r.result?.result?.value ?? r.result?.result, errors }, null, 1));
cleanup();
process.exit(errors.length ? 3 : 0);
