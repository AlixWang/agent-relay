// Behavioural test for the command console's shared state and route wiring.
//
//   node internal/web/ui/_command-state.test.mjs
//
// Reported from production: 群聊频道 (0) / 助手私聊 (0) although /admin/rooms
// returned the room with five members. Cause: Alpine.data() is a factory, so the
// three x-data="commandApp" roots (page + create-room dialog + member dialog)
// each built their own state, and the module-level "active app" ended up being
// the LAST one initialized — a dialog. refreshCommand() (fired on entering the
// route) therefore filled the dialog's state while the rendered page section
// stayed empty, and the page's own init() only loaded when the console was
// booted directly onto #command.
//
// This drives the real js/command.js with a stubbed DOM/API and asserts the
// invariants: one shared state object, idempotent init, and that entering the
// route fills THAT object.
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

// ---- DOM / browser stubs -------------------------------------------------
const els = {};
const listeners = { element: [], document: [] };
const mkEl = (id) => ({
  id, hidden: false, className: '', textContent: '', innerHTML: '', dataset: {}, style: {},
  scrollTop: 0, scrollHeight: 100, clientHeight: 100, classList: { toggle() {}, add() {}, remove() {} },
  addEventListener: (t) => listeners.element.push(`${id}:${t}`),
  getBoundingClientRect: () => ({ top: 0, left: 0, right: 100, bottom: 100, width: 100, height: 100 }),
  showModal() { this.open = true; }, close() { this.open = false; }, focus() {}, querySelector: () => null,
  querySelectorAll: () => [], appendChild() {}, remove() {}, setAttribute() {},
});
const document = {
  getElementById: (id) => (els[id] ||= mkEl(id)),
  querySelector: () => mkEl('nested'),
  querySelectorAll: () => [],
  createElement: (t) => mkEl(t),
  addEventListener: (t) => listeners.document.push(t),
  body: mkEl('body'), documentElement: mkEl('html'), hidden: false,
};
class HTMLDialogElement { showModal() {} close() {} }
const timers = new Map();
let timerSeq = 0;

globalThis.document = document;
globalThis.HTMLDialogElement = HTMLDialogElement;
globalThis.window = { addEventListener() {}, isSecureContext: true, matchMedia: () => ({ matches: false, addEventListener() {} }) };
globalThis.location = { href: 'https://relay.test/#members', hash: '#members', pathname: '/', search: '' };
globalThis.history = { replaceState() {} };
globalThis.requestAnimationFrame = (fn) => fn();
globalThis.setInterval = (fn) => { timers.set(++timerSeq, fn); return timerSeq; };
globalThis.clearInterval = (id) => timers.delete(id);

// Canned responses, mirroring the live API shapes.
const ROOM = {
  id: 'grp_chat', name: '聊天群', note: '', created_at: 1791554493, archived: false, count: 5,
  members: [{ id: 'fe', status: 'active', online: true }, { id: 'erdan', status: 'active', online: true }],
  last_seq: 155, last_sender: 'fe', last_kind: 'chat', last_at: 1791555629, last_preview: '收到 @fe，人在。',
};
const PEERS = ['163boy', 'Mmei', 'erdan', 'fe', 'xiaoe'].map((id) => ({ id, online: true, agent_type: 'muse', prompt_version: 12 }));
const calls = [];
globalThis.fetch = async (url) => {
  calls.push(url);
  const body = url.startsWith('/admin/rooms') ? { ok: true, rooms: [ROOM] }
    : url.startsWith('/admin/inbox') ? { ok: true, unread: 0, items: [] }
      : url.startsWith('/admin/peers') ? { ok: true, peers: PEERS }
        : { ok: true };
  return { ok: true, status: 200, statusText: 'OK', json: async () => body };
};

// ---- import the real module (it must be syntax-valid ESM too) ------------
const mod = await import(pathToFileURL(path.join(here, 'js', 'command.js')).href);
const { commandApp, refreshCommand, stopCommand } = mod;

const assert = (cond, msg) => {
  if (!cond) {
    console.error('FAIL: ' + msg);
    process.exit(1);
  }
};

// 1. One state object for every Alpine root (page + 2 dialogs).
const page = commandApp();
const dialogA = commandApp();
// Alpine injects this magic into every x-data scope; the test calls the code
// outside Alpine, so provide it here.
page.$nextTick = (fn) => fn && fn();
assert(page === dialogA, 'x-data="commandApp" roots must share ONE state object, got two');
assert(typeof page.loadSessions === 'function', 'shared state must expose loadSessions');

// 2. init() is wiring, not a per-root side effect: Alpine calls it once per root.
const before = listeners.document.length + listeners.element.length;
page.init();
page.init();
page.init();
const after = listeners.document.length + listeners.element.length;
assert(after - before === 3, `init() must wire the DOM once (dialog + dialog + visibility), wired ${after - before} listeners`);
assert(listeners.element.filter((l) => l.startsWith('dlg')).length === 2, 'each dialog must be set up exactly once');

// 3. Entering the route fills the object the page renders — the actual bug.
location.hash = '#command?room=grp_chat';
await refreshCommand();
await new Promise((r) => setTimeout(r, 10));
assert(page.rooms.length === 1 && page.rooms[0].id === 'grp_chat', 'rooms from /admin/rooms must land on the shared state, got ' + page.rooms.length);
assert(page.rooms[0].members.length === 2, 'room members must survive the round trip');
assert(page.peers.length === 5, 'peers must land on the shared state, got ' + page.peers.length);
assert(page.activeTarget.type === 'room' && page.activeTarget.id === 'grp_chat', 'deep link #command?room=… must select the room');
assert(calls.some((u) => u.startsWith('/admin/rooms')), 'the SPA must actually call /admin/rooms on route entry');

// 4. Polling: one timer, no leaks, and it stops when the route is left.
assert(timers.size === 1, 'expected exactly one poll timer after entering the route, got ' + timers.size);
await refreshCommand();
assert(timers.size === 1, 're-entering the route must not stack poll timers, got ' + timers.size);
stopCommand();
assert(timers.size === 0, 'leaving the route must stop polling');

console.log('command state: all checks passed');
