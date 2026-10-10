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

// 5. Read receipts: who counts for a room message. A member only sees messages
// sent after they joined (start_seq), and a sender obviously read their own.
page.rooms = [
  {
    id: 'grp_chat', name: '聊天群', archived: false,
    members: [
      { id: 'alice', start_seq: 1 },
      { id: 'bob', start_seq: 1 },
      { id: 'fe', start_seq: 1 },
      { id: 'late', start_seq: 200 },
    ],
  },
  { id: 'grp_old', name: '旧群', archived: true, members: [{ id: 'alice', start_seq: 1 }] },
];
page.activeTarget = { type: 'room', id: 'grp_chat', name: '聊天群', subtitle: '' };

const msg = { seq: 155, from: 'operator', acked_by: ['alice', 'bob'], kind: 'task' };
assert(page.ackEligible(msg).join(',') === 'alice,bob,fe', 'late joiners must not be counted as unread, got ' + page.ackEligible(msg));
assert(page.ackReaders(msg).join(',') === 'alice,bob', 'readers: ' + page.ackReaders(msg));
assert(page.ackPending(msg).join(',') === 'fe', 'pending must be exactly the members who have not acked: ' + page.ackPending(msg));
assert(page.ackLabel(msg) === '已读 2/3', 'partial label: ' + page.ackLabel(msg));
assert(page.ackTone(msg) === 'warn', 'partial tone: ' + page.ackTone(msg));
assert(page.ackLateSent(msg).join(',') === 'late', 'the late joiner must be reported separately: ' + page.ackLateSent(msg));

const allRead = { seq: 155, from: 'operator', acked_by: ['alice', 'bob', 'fe'] };
assert(page.ackLabel(allRead) === '全部已读' && page.ackTone(allRead) === 'ok', 'all-read label/tone: ' + page.ackLabel(allRead) + '/' + page.ackTone(allRead));
assert(page.ackPending(allRead).length === 0, 'all-read must have no pending');

const noneRead = { seq: 155, from: 'operator', acked_by: [] };
assert(page.ackLabel(noneRead) === '无人已读' && page.ackPending(noneRead).length === 3, 'none-read: ' + page.ackLabel(noneRead) + '/' + page.ackPending(noneRead).length);

const ownMsg = { seq: 210, from: 'alice', acked_by: ['bob'] };
assert(page.ackEligible(ownMsg).join(',') === 'bob,fe,late', 'the sender must never show up as unread: ' + page.ackEligible(ownMsg));
assert(page.ackLabel(ownMsg) === '已读 1/3', 'sender-excluded label: ' + page.ackLabel(ownMsg));

// A room opened straight from the thread payload (members as ids, no start_seq)
// still works: everyone counts.
page.rooms = [];
page.activeRoomData = { id: 'grp_chat', members: ['alice', 'bob'], archived: false };
assert(page.ackEligible({ seq: 5, from: 'operator', acked_by: ['alice'] }).join(',') === 'alice,bob', 'thread-only fallback: ' + page.ackEligible({ seq: 5, from: 'operator', acked_by: ['alice'] }));

// A private reply living in the room thread (to=peer / to=operator, seen live in
// grp_chat seq 165): only the addressee can read or ack it, so counting the
// whole room showed "已读 1/4" forever and read like ack laziness.
page.rooms = [{ id: 'grp_chat', name: '聊天群', archived: false, members: [{ id: 'alice', start_seq: 1 }, { id: 'bob', start_seq: 1 }, { id: 'carol', start_seq: 1 }, { id: 'dave', start_seq: 1 }] }];
const dm = { seq: 165, from: 'alice', to: 'bob', acked_by: ['bob'], kind: 'result' };
assert(page.roomMessage(dm) === false, 'a reply to one member is not a room message');
assert(page.ackEligible(dm).join(',') === 'bob', 'only the addressee counts for a private reply: ' + page.ackEligible(dm));
assert(page.ackLabel(dm) === '全部已读' && page.ackTone(dm) === 'ok', 'an acked DM is fully read: ' + page.ackLabel(dm) + '/' + page.ackTone(dm));
assert(page.ackPending(dm).length === 0 && page.ackLateSent(dm).length === 0, 'a private reply has no room-wide pending/late rows');
const dmUnread = { seq: 166, from: 'alice', to: 'bob', acked_by: [] };
assert(page.ackLabel(dmUnread) === '无人已读' && page.ackPending(dmUnread).join(',') === 'bob', 'an unanswered DM is the addressee pending: ' + page.ackLabel(dmUnread) + '/' + page.ackPending(dmUnread));
const dmOp = { seq: 167, from: 'alice', to: 'operator', acked_by: [] };
assert(page.roomMessage(dmOp) === false && page.ackEligible(dmOp).length === 0, 'operator reads ride the inbox watermark: no per-message receipt');
assert(page.ackLabel(dmOp) === '' && page.ackTone(dmOp) === '', 'a to-operator reply shows no receipt badge');
const dmSelf = { seq: 168, from: 'bob', to: 'bob', acked_by: [] };
assert(page.ackEligible(dmSelf).length === 0, 'nobody acks their own private reply');

// 6. Dissolved rooms: out of the working list, reachable under 已解散.
page.rooms = [
  { id: 'grp_chat', name: '聊天群', archived: false, members: [{ id: 'alice', start_seq: 1 }] },
  { id: 'grp_old', name: '旧群', archived: true, members: [{ id: 'alice', start_seq: 1 }] },
];
page.searchQuery = '';
assert(page.filteredRooms().map((r) => r.id).join(',') === 'grp_chat', 'active list: ' + page.filteredRooms().map((r) => r.id));
assert(page.filteredArchivedRooms().map((r) => r.id).join(',') === 'grp_old', 'dissolved list: ' + page.filteredArchivedRooms().map((r) => r.id));
page.searchQuery = 'old';
assert(page.filteredRooms().length === 0 && page.filteredArchivedRooms().length === 1, 'search must apply to both groups');
page.searchQuery = '';

// The pane knows the room is gone: composer hidden, history readable.
page.activeTarget = { type: 'room', id: 'grp_old', name: '旧群', subtitle: '' };
page.activeRoomData = null;
assert(page.roomDissolved() === true, 'a dissolved room must be recognised from the room list');
page.activeRoomData = { id: 'grp_old', members: ['alice'], archived: true };
assert(page.roomDissolved() === true, 'a dissolved room must be recognised from the thread payload');
page.activeTarget = { type: 'room', id: 'grp_chat', name: '聊天群', subtitle: '' };
page.activeRoomData = { id: 'grp_chat', members: ['alice'], archived: false };
assert(page.roomDissolved() === false, 'a live room must not look dissolved');

console.log('command state: all checks passed');
