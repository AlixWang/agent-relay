// Behavioural test for the console's update panel (the update section of app.js).
//
// The panel is the screen an operator watches while the service restarts
// underneath them. Go tests can only assert that app.js *contains* the right
// markers; the logic itself (auto-reload exactly once, no reload loop, staying
// informative but quiet during the restart) needs the real code to run.
//
//   node internal/web/ui/_update-panel.test.mjs
//
// The panel section is sliced out of app.js at run time and imported as a
// throwaway module with a stubbed DOM/API, so this test can never drift from
// the code it checks. The leading underscore in the file name keeps it out of
// the embedded assets (//go:embed ui/* skips _*), so browsers never see it.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const appJs = fs.readFileSync(path.join(here, 'app.js'), 'utf8');
const from = appJs.indexOf('/* Update job progress.');
const to = appJs.indexOf("\n$('updJobToggle').onclick");
const panel = from >= 0 && to > from ? appJs.slice(from, to) : '';
if (!panel.includes('updPoll') || !panel.includes('location.reload()')) {
  console.error('FAIL: cannot locate the update panel section in app.js (renamed?)');
  process.exit(1);
}

// The panel code runs inside a module that carries the stubs it expects.
const harness = `
export const ctl = {
  reloads: 0, toasts: [], apiQueue: [], apiError: false, timers: new Map(), timerSeq: 0,
  refreshCount: 0, els: {},
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const mkEl = () => ({ hidden: false, className: '', textContent: '', innerHTML: '', dataset: {}, scrollTop: 0, clientHeight: 100, scrollHeight: 100 });
export const $ = (id) => (ctl.els[id] ||= mkEl());
const esc = (s) => String(s ?? '');
const icon = () => '<svg/>';
const fmtTime = () => '2026-01-01 00:00';
const toast = (m, k) => ctl.toasts.push([m, k]);
const refreshSystem = () => ctl.refreshCount++;
const location = { reload: () => ctl.reloads++ };
const setInterval = (fn) => { ctl.timers.set(++ctl.timerSeq, fn); return ctl.timerSeq; };
const clearInterval = (id) => { ctl.timers.delete(id); };
const api = async () => { if (ctl.apiError) throw new Error('ECONNREFUSED'); return ctl.apiQueue.shift(); };
let updJobTimer = null;

${panel}

export { renderUpdJob, updPoll, updPollStart, updPollStop, updIsFinal, sleep };
export const seenAdd = (id) => updSeenInFlight.add(id);
export const seenReset = () => { updSeenInFlight = new Set(); };
`;
const tmp = path.join(os.tmpdir(), `ar-panel-under-test-${process.pid}.mjs`);
fs.writeFileSync(tmp, harness);
const m = await import(pathToFileURL(tmp).href);
fs.unlinkSync(tmp);

const { ctl } = m;
const assert = (cond, msg) => {
  if (!cond) {
    console.error('FAIL: ' + msg);
    process.exit(1);
  }
};
const $ = (id) => m.$(id);
const timers = () => ctl.timers.size;
const toasts = () => ctl.toasts;
const queue = (jobs) => { ctl.apiQueue = jobs; };
const failApi = (on) => { ctl.apiError = on; };
const reset = () => { ctl.reloads = 0; ctl.toasts = []; ctl.refreshCount = 0; };
const sleep = m.sleep;

// 1. success of a job this page view watched → the page refreshes itself
m.seenAdd('j1');
queue([{ job: { id: 'j1', status: 'ok', version: 'v0.15.4', elapsed_secs: 42, confirmed: true, phases: [{ name: 'X', status: 'done' }], log: 'UPDATE_RESULT ok v0.15.4' } }]);
await m.updPoll();
assert(ctl.reloads === 0, 'must not reload before the success toast is readable');
assert(timers() === 0, 'polling must stop once the job is final');
assert(toasts().some(([, k]) => k === 'ok'), 'success toast missing');
await sleep(1800);
assert(ctl.reloads === 1, 'success must reload the page automatically, got ' + ctl.reloads);

// 2. opening the panel on an already finished job must not reload (no loop)
reset();
m.seenReset(); // a fresh page load remembers nothing
queue([{ job: { id: 'j1', status: 'ok', version: 'v0.15.4', phases: [], log: '' } }]);
await m.updPoll();
await sleep(1800);
assert(ctl.reloads === 0, 'reloading a finished job must not reload again (loop!)');

// 3. failure → no reload, error toast, panel refreshed
reset();
queue([{ job: { id: 'j2', status: 'rolled_back', version: 'v0.15.4', phases: [], log: 'health failed' } }]);
await m.updPoll();
await sleep(1800);
assert(ctl.reloads === 0 && toasts().some(([, k]) => k === 'bad'), 'a rolled-back job must report, not reload');
assert(ctl.refreshCount === 1, 'the panel must refresh after a final state');

// 4. the gateway restarts mid-update: stay quiet, keep polling
failApi(true);
reset();
m.updPollStart();
await m.updPoll();
assert($('updJobState').textContent === '服务重启中', 'restart window must be labelled, got ' + $('updJobState').textContent);
assert(timers() === 1, 'must keep polling through the restart');
assert(ctl.reloads === 0 && toasts().length === 0, 'the restart window must not spam the operator');

// 5. the status endpoint answers but the job is momentarily unknown
failApi(false);
queue([{}, {}]);
await m.updPoll();
assert($('updJobState').textContent === '服务重启中', 'a jobless answer still means restarting');

// 6. in-flight job: live log, phase checklist, polling continues
queue([{
  job: {
    id: 'j3', status: 'running', version: 'v0.15.4', elapsed_secs: 7,
    phases: [{ name: '下载并校验 release 文件', status: 'active', detail: 'GET url' }],
    log: 'STEP download GET url',
  },
}]);
await m.updPoll();
assert($('updJob').hidden === false && $('updLog').textContent === 'STEP download GET url', 'the helper log must be rendered live');
assert($('updSteps').innerHTML.includes('下载并校验') && $('updSteps').innerHTML.includes('active'), 'the phase checklist must be rendered');
assert($('updJobHint').hidden === true, 'no hint on a healthy job');
assert($('updJobMeta').textContent.includes('v0.15.4') || $('updJobVer').textContent === 'v0.15.4', 'the target version must be visible');
assert(timers() === 1, 'must still be polling an in-flight job');

// 7. a stuck job surfaces the server's hint instead of looking busy forever
queue([{
  job: {
    id: 'j3', status: 'queued', version: 'v0.15.4', hint: '已排队 3 分仍未取件：检查 agent-relay-update.timer',
    phases: [{ name: '排队等待 root timer 取件', status: 'active' }], log: '',
  },
}]);
await m.updPoll();
assert($('updJobHint').hidden === false && $('updJobHint').innerHTML.includes('agent-relay-update.timer'), 'the server hint must be shown');

// 8. the poller must not leak timers
m.updPollStart();
m.updPollStart();
assert(timers() === 1, 'expected exactly one poll timer, got ' + timers());
m.updPollStop();
assert(timers() === 0, 'stop must clear the timer');

console.log('update panel logic: all checks passed');
