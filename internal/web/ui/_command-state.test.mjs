// Behavioural test for the command console's state and Alpine roots.
//
//   node internal/web/ui/_command-state.test.mjs
//
// This test exists because the command console broke twice in production:
//
//  1. 群聊频道 (0) / 助手私聊 (0) while /admin/rooms returned the room with its
//     members. Alpine.data() is a factory and the page carried THREE
//     x-data="commandApp" roots (page + 2 dialogs), so entering the route filled
//     the state of whichever instance init()ed last — a dialog — while the
//     rendered page section stayed empty.
//  2. The attempted fix (one object shared by all roots) broke the console
//     outright: Alpine stamps magics with a NON-configurable defineProperty on
//     every x-data root's object, so the second root threw
//     "Cannot redefine property: $nextTick" out of Alpine.start().
//
// Hence the invariants tested here: a factory that always returns a distinct
// object (safe for multiple roots), exactly one root in the markup (checked by
// TestConsoleCommandRootIsSingle in Go), and route entry filling the instance the
// page renders.
import fs from 'node:fs';
import path from 'node:path';
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
  querySelector: () => mkEl('nested'), querySelectorAll: () => [],
  showModal() { this.open = true; }, close() { this.open = false; }, focus() {},
  appendChild() {}, remove() {}, setAttribute() {},
});
const document = {
  getElementById: (id) => (els[id] ||= mkEl(id)),
  querySelector: () => null,
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

// ---- import the real module (also proves it is valid ESM) ----------------
const mod = await import(pathToFileURL(path.join(here, 'js', 'command.js')).href);
const { commandApp, refreshCommand, stopCommand } = mod;

const assert = (cond, msg) => {
  if (!cond) {
    console.error('FAIL: ' + msg);
    process.exit(1);
  }
};

// Alpine's injectMagics, verbatim in effect: a NON-configurable property per
// x-data root. If the factory ever hands out the same object twice, the second
// call throws exactly like production did.
const alpineInjectMagics = (obj) => {
  for (const name of ['nextTick', 'el', 'refs', 'store', 'watch', 'dispatch', 'root', 'data', 'id']) {
    Object.defineProperty(obj, '$' + name, { get() { return () => {}; }, enumerable: false });
  }
};

// 1. Distinct state per root: safe for several roots, and the crash guard.
// Alpine injects $nextTick & friends into every x-data scope; the test drives the
// code outside Alpine, so provide it on the instance under test before Alpine's
// (non-configurable) stamping would make it read-only.
const page = commandApp();
page.$nextTick = (fn) => fn && fn();
const secondRoot = commandApp();
assert(page !== secondRoot, 'commandApp() must return a fresh object per Alpine root: a shared object throws "Cannot redefine property: $nextTick" out of Alpine.start()');
alpineInjectMagics(page);
alpineInjectMagics(secondRoot);

// 2. init() records the instance the page renders. Alpine calls it once per
// x-data root, and the markup is guarded to have exactly one (see
// TestConsoleCommandRootIsSingle), which is what keeps the route handlers and
// the rendered state the same object.
page.init();
assert(listeners.element.filter((l) => l.startsWith('dlg')).length === 2, 'both dialogs must be wired by init()');
assert(listeners.document.includes('visibilitychange'), 'polling must pause when the tab is hidden');

// 3. Entering the route fills THAT instance — the original bug.
location.hash = '#command?room=grp_chat';
await refreshCommand();
await new Promise((r) => setTimeout(r, 10));
assert(page.rooms.length === 1 && page.rooms[0].id === 'grp_chat', 'rooms from /admin/rooms must land on the rendered instance, got ' + page.rooms.length);
assert(page.rooms[0].members.length === 2, 'room members must survive the round trip');
assert(page.peers.length === 5, 'peers must land on the rendered instance, got ' + page.peers.length);
assert(page.activeTarget.type === 'room' && page.activeTarget.id === 'grp_chat', 'deep link #command?room=… must select the room');
assert(calls.some((u) => u.startsWith('/admin/rooms')), 'the SPA must actually call /admin/rooms on route entry');

// 4. Polling: one timer, no leaks, stops when the route is left.
assert(timers.size === 1, 'expected exactly one poll timer after entering the route, got ' + timers.size);
await refreshCommand();
assert(timers.size === 1, 're-entering the route must not stack poll timers, got ' + timers.size);
stopCommand();
assert(timers.size === 0, 'leaving the route must stop polling');

console.log('command state: all checks passed');
