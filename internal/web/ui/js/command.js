// Command & Group Chat Console module (Alpine.js component)
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, avatar, toast, confirmAction, setupDialog, copyToClipboard } from './utils.js';
import { renderMessage as renderMessageHTML, messageIsLong } from './message.js';
import { currentRoute, onRoute } from './router.js';
import { refreshStats } from './auth.js';

let commandPollTimer = null;
let activeCommandApp = null;

// Virtual message stream. The console used to hand every thread row to Alpine
// x-for (1036 rows in op/Lisa, each running Markdown + a document listener for
// the ack popover), which is what made the page take 15s. Only a window of
// rows around the viewport is rendered now; spacers stand in for the rest so
// the scrollbar still reflects the whole loaded history.
const CMD_WINDOW = 50;       // rendered rows around the viewport
const CMD_OVERSCAN = 8;      // look-ahead rows before the window slides
const CMD_EST_HEIGHT = 88;   // px, until a row has been measured
const CMD_GAP = 12;          // .cmd-stream gap
const CMD_BOTTOM_SLACK = 32; // "already at the bottom" tolerance

// The command console is ONE Alpine root: the page section carries
// x-data="commandApp", and the create-room / member dialogs live inside that
// section so they inherit its scope (they need the same peers/rooms state).
//
// Both halves of that sentence were bugs (群聊频道/助手私聊 showed 0 while
// /admin/rooms answered fine, and a later attempt to share one object across
// roots broke the whole console):
//  - Separate x-data="commandApp" roots each got their own state, and the
//    module-level "active app" ended up being the LAST one initialized — a
//    dialog. So entering the route filled the dialog's state while the rendered
//    page section stayed empty.
//  - Returning one shared object to several roots is worse: Alpine calls
//    Object.defineProperty(obj, '$' + magic, …) (non-configurable) for every
//    x-data root, so the second root throws "Cannot redefine property:
//    $nextTick" and Alpine.start() dies, leaving a blank console.
// Hence: one root (enforced by TestConsoleCommandRootIsSingle), and a factory
// that always returns a fresh state object.
export function commandApp() {
  return createCommandState();
}

export function refreshCommand() {
  if (!activeCommandApp) return;
  activeCommandApp.handleHashParams();
  activeCommandApp.loadSessions();
  activeCommandApp.startPolling();
}

export function stopCommand() {
  if (activeCommandApp) activeCommandApp.stopPolling();
  // setRoute() closes the main sidebar (nav-open) on every navigation;
  // the session drawer follows the same contract.
  if (activeCommandApp) activeCommandApp.sessionsOpen = false;
}

function createCommandState() {
  return {
    // Session state
    searchQuery: '',
    inboxUnread: 0,
    inboxItems: [],
    // Unread replies per assistant (GET /admin/inbox unread_by_peer): the
    // badges on the 助手私聊 list. Opening a DM marks that peer read.
    peerUnread: {},
    rooms: [],
    peers: [],
    
    // Active session target
    activeTarget: {
      type: 'broadcast', // 'inbox' | 'broadcast' | 'room' | 'peer'
      id: '*',
      name: '全体广播',
      subtitle: '发送通知或广播给所有助手',
    },
    
    // Active room metadata (when activeTarget.type === 'room')
    activeRoomData: null,
    
    // Message stream. `messages` is the loaded model (newest page + older
    // pages on demand); `visibleMessages` is the ~50-row render window.
    messages: [],
    windowStart: 0,
    windowSize: CMD_WINDOW,
    msgHeights: {}, // seq -> measured row height in px
    hasOlder: false,
    loadingOlder: false,
    oldestSeq: 0,
    newestSeq: 0,
    totalMessages: 0,
    loadingMessages: false,

    // Scroll: stickToBottom is whether the reader was already at the bottom
    // before the latest refresh (so a poll may follow). newBelow is the
    // "new messages below" chip shown while they are reading history.
    stickToBottom: true,
    newBelow: false,

    // Read receipts: which message's popover is open (seq), and whether the
    // dissolved-rooms group in the session list is expanded.
    ackOpen: null,
    showArchived: false,

    // Long-message reading state, keyed by message seq/id. Collapsed by
    // default (see needsCollapse) so one giant report does not push the rest
    // of the conversation off-screen.
    expandedMsgs: {},

    // Mobile drawer for the session list (<=900px): toggled by the menu
    // button in .cmd-chat-header, closed on session select.
    sessionsOpen: false,
    
    // Composer state
    payload: '',
    kind: 'task', // 'task' | 'chat'
    sending: false,

    // Dialog state
    newRoom: {
      id: '',
      name: '',
      members: [],
    },
    newMemberId: '',

    init() {
      // One x-data root, so this runs once: remember the instance that the
      // page renders, for the route handlers to drive.
      activeCommandApp = this;

      // Setup modals
      setupDialog($('dlgCreateRoom'));
      setupDialog($('dlgRoomMembers'));

      // Target from the URL (e.g. #command?room=grp_ops) — no data load here:
      // entering the route goes through refreshCommand(), and booting straight
      // onto #command goes through setRoute() → the same path. Loading here as
      // well only created a race that left the page empty.
      this.handleHashParams();

      // Window visibility change handling (pause polling when hidden)
      document.addEventListener('visibilitychange', () => {
        if (document.hidden) {
          this.stopPolling();
        } else if (currentRoute === 'command') {
          this.startPolling();
        }
      });
    },

    handleHashParams() {
      const hash = location.hash || '';
      if (!hash.startsWith('#command')) return;
      const qIdx = hash.indexOf('?');
      if (qIdx !== -1) {
        const params = new URLSearchParams(hash.slice(qIdx));
        const room = params.get('room');
        const peer = params.get('peer');
        const view = params.get('view');
        if (room) {
          this.selectRoom({ id: room, name: room });
        } else if (peer) {
          this.selectPeer({ id: peer, display_name: peer });
        } else if (view === 'inbox') {
          this.selectInbox();
        }
      }
    },

    startPolling() {
      this.stopPolling();
      commandPollTimer = setInterval(() => {
        if (currentRoute === 'command' && !$('app').hidden && !document.hidden) {
          this.pollUpdate();
        }
      }, 3000);
    },

    stopPolling() {
      if (commandPollTimer) {
        clearInterval(commandPollTimer);
        commandPollTimer = null;
      }
    },

    async pollUpdate() {
      // 1. Silent sessions refresh
      try {
        const [roomsRes, inboxRes] = await Promise.allSettled([
          api('/admin/rooms?archived=1'),
          api('/admin/inbox?page_size=10'),
        ]);
        if (roomsRes.status === 'fulfilled' && roomsRes.value?.rooms) {
          this.rooms = roomsRes.value.rooms;
        }
        if (inboxRes.status === 'fulfilled' && inboxRes.value) {
          this.applyInbox(inboxRes.value);
        }
      } catch (_) {}

      // 2. Silent message stream refresh. Room/DM threads poll forward from the
      // newest seq they already hold; only the inbox/broadcast (small, already
      // bounded payloads) still refresh their page.
      if (this.activeTarget.type === 'room' || this.activeTarget.type === 'peer') {
        await this.pollThreadMessages();
      } else {
        await this.loadActiveMessages(true);
      }
    },

    async loadSessions() {
      try {
        const [roomsRes, inboxRes, peersRes] = await Promise.allSettled([
          api('/admin/rooms?archived=1'),
          api('/admin/inbox?page_size=30'),
          api('/admin/peers?page_size=100'),
        ]);
        if (roomsRes.status === 'fulfilled' && roomsRes.value) {
          this.rooms = roomsRes.value.rooms || [];
        }
        if (inboxRes.status === 'fulfilled' && inboxRes.value) {
          this.applyInbox(inboxRes.value);
        }
        if (peersRes.status === 'fulfilled' && peersRes.value) {
          this.peers = peersRes.value.peers || [];
        }

        // If target is a room, match room name from list
        if (this.activeTarget.type === 'room') {
          const found = this.rooms.find((r) => r.id === this.activeTarget.id);
          if (found) {
            this.activeTarget.name = found.name ? `${found.name} (${found.id})` : found.id;
            this.activeTarget.subtitle = `${(found.members || []).length} 名成员 · 群聊频道`;
          }
        }
        await this.loadActiveMessages(false);
      } catch (err) {
        toast('加载指挥台会话失败：' + err.message, 'bad');
      }
    },

    // Inbox counters from GET /admin/inbox or POST /admin/inbox/read.
    applyInbox(res, withItems = true) {
      this.inboxUnread = res.unread || 0;
      this.peerUnread = res.unread_by_peer || {};
      if (withItems) this.inboxItems = res.items || [];
    },

    peerUnreadCount(peerId) {
      // The open DM is being read right now; its badge would only flash
      // until markPeerRead() lands.
      if (this.activeTarget.type === 'peer' && this.activeTarget.id === peerId) return 0;
      return (this.peerUnread && this.peerUnread[peerId]) || 0;
    },

    // Latest unread reply as the list preview, like any IM; otherwise who the
    // assistant is.
    peerPreview(p) {
      if (this.peerUnreadCount(p.id) > 0) {
        const last = (this.inboxItems || []).find((m) => m.from === p.id);
        if (last && last.payload) return String(last.payload).replace(/\s+/g, ' ').slice(0, 80);
      }
      return p.display_name || p.agent_type || '在线助手';
    },

    // Viewing a DM reads it: move that peer's watermark to the newest reply
    // shown. Skipped when the server already counts nothing unread.
    async markPeerRead(peerId) {
      if (!(this.peerUnread && this.peerUnread[peerId] > 0)) return;
      const seq = this.messages
        .filter((m) => m.from === peerId && m.to === 'operator')
        .reduce((max, m) => Math.max(max, m.seq || 0), 0);
      if (!seq) return;
      try {
        const res = await api('/admin/inbox/read', {
          method: 'POST',
          body: JSON.stringify({ peer: peerId, seq }),
        });
        this.applyInbox(res, false);
        refreshStats();
      } catch (_) {}
    },

    // Session Switchers
    selectInbox() {
      this.activeTarget = {
        type: 'inbox',
        id: 'inbox',
        name: '收件箱 (Operator)',
        subtitle: '助手给管理员发送的所有汇报与回复',
      };
      this.activeRoomData = null;
      this.ackOpen = null;
      this.sessionsOpen = false;
      if (location.hash !== '#command?view=inbox') {
        history.replaceState(null, '', '#command?view=inbox');
      }
      this.loadActiveMessages(false);
    },

    selectBroadcast() {
      this.activeTarget = {
        type: 'broadcast',
        id: '*',
        name: '全体广播',
        subtitle: '向所有在线助手广播一条指令或通知',
      };
      this.activeRoomData = null;
      this.sessionsOpen = false;
      if (location.hash !== '#command') {
        history.replaceState(null, '', '#command');
      }
      this.loadActiveMessages(false);
    },

    selectRoom(room) {
      this.activeTarget = {
        type: 'room',
        id: room.id,
        name: room.name ? `${room.name} (${room.id})` : room.id,
        subtitle: `${(room.members || []).length} 名成员 · 群聊频道`,
      };
      if (location.hash !== '#command?room=' + encodeURIComponent(room.id)) {
        history.replaceState(null, '', '#command?room=' + encodeURIComponent(room.id));
      }
      this.sessionsOpen = false;
      this.loadActiveMessages(false);
    },

    selectPeer(peer) {
      this.activeTarget = {
        type: 'peer',
        id: peer.id,
        name: `@ ${peer.id}`,
        subtitle: `${peer.display_name || peer.agent_type || '助手'} · 直发私聊`,
      };
      this.activeRoomData = null;
      if (location.hash !== '#command?peer=' + encodeURIComponent(peer.id)) {
        history.replaceState(null, '', '#command?peer=' + encodeURIComponent(peer.id));
      }
      this.sessionsOpen = false;
      this.loadActiveMessages(false);
    },

    // ---- virtual message stream -------------------------------------
    // The reactive model is the full loaded page set; the DOM only ever holds
    // visibleMessages (~50 rows) plus two spacer blocks. Heights are measured
    // after each render and cached by seq, so scrolling through Markdown of
    // wildly different sizes keeps the scrollbar honest.
    get visibleMessages() {
      return this.messages.slice(this.windowStart, this.windowStart + this.windowSize);
    },

    get topPad() {
      return this.messageOffsetAt(this.windowStart);
    },

    get bottomPad() {
      const rendered = Math.min(this.windowSize, Math.max(0, this.messages.length - this.windowStart));
      const end = this.windowStart + rendered;
      return Math.max(0, this.messageContentHeight() - this.messageOffsetAt(end));
    },

    get canLoadOlder() {
      return (this.activeTarget.type === 'room' || this.activeTarget.type === 'peer') && this.hasOlder;
    },

    messageHeight(m) {
      const h = m && this.msgHeights[m.seq];
      return Number.isFinite(h) && h > 0 ? h : CMD_EST_HEIGHT;
    },

    messageContentHeight() {
      const n = this.messages.length;
      if (!n) return 0;
      let total = CMD_GAP * (n - 1);
      for (const m of this.messages) total += this.messageHeight(m);
      return total;
    },

    messageOffsetAt(index) {
      const end = Math.max(0, Math.min(index, this.messages.length));
      let off = 0;
      for (let i = 0; i < end; i++) off += this.messageHeight(this.messages[i]) + CMD_GAP;
      return off;
    },

    messageIndexAtOffset(top) {
      let off = 0;
      for (let i = 0; i < this.messages.length; i++) {
        const h = this.messageHeight(this.messages[i]);
        if (off + h + CMD_GAP > top) return i;
        off += h + CMD_GAP;
      }
      return Math.max(0, this.messages.length - 1);
    },

    maxMessageSeq() {
      return this.messages.reduce((max, m) => Math.max(max, m.seq || 0), 0);
    },

    resetMessageWindow() {
      this.messages = [];
      this.msgHeights = {};
      this.windowStart = 0;
      this.hasOlder = false;
      this.loadingOlder = false;
      this.oldestSeq = 0;
      this.newestSeq = 0;
      this.totalMessages = 0;
    },

    setLoadedMessages(items, meta = {}) {
      this.messages = items;
      this.totalMessages = Number.isFinite(meta.total) ? meta.total : items.length;
      this.hasOlder = !!meta.has_more;
      this.oldestSeq = meta.oldest_seq || (items[0] && items[0].seq) || 0;
      this.newestSeq = meta.newest_seq || (items.length && items[items.length - 1].seq) || 0;
      this.windowStart = Math.max(0, items.length - this.windowSize);
      this.newBelow = false;
    },

    threadRoot() {
      if (this.activeTarget.type === 'room') return this.activeTarget.id;
      if (this.activeTarget.type === 'peer') return 'op/' + this.activeTarget.id;
      return '';
    },

    threadURL() {
      const root = this.threadRoot();
      return root ? '/admin/messages?thread=' + encodeURIComponent(root) : '';
    },

    // Server thread item -> stream row. Room and DM share the same shape.
    threadItem(m) {
      return {
        seq: m.seq,
        id: m.id,
        from: m.sender,
        to: m.recipient,
        kind: m.kind,
        payload: m.payload,
        time: m.created_at,
        acked_by: m.acked_by || [],
        read: m.read,
        total: m.total,
        approval_state: m.approval_state,
      };
    },

    // Message loading
    async loadActiveMessages(silent = false) {
      if (!silent) {
        this.loadingMessages = true;
        this.resetMessageWindow();
      }
      // A poll only follows when the reader was already resting at the bottom;
      // otherwise their place in history must survive the refresh.
      const stream = $('cmdStreamContainer');
      const wasAtBottom = !stream || this.isAtBottom(stream);
      const prevMaxSeq = this.maxMessageSeq();
      this.stickToBottom = wasAtBottom;
      try {
        if (this.activeTarget.type === 'inbox') {
          const r = await api('/admin/inbox?page_size=50');
          this.applyInbox(r, false);
          // Inbox pages newest-first; the stream reads top-down and rests at
          // the bottom, so render oldest-first like every other timeline.
          this.setLoadedMessages((r.items || []).slice().reverse().map((m) => ({
            seq: m.seq,
            id: m.id,
            from: m.from,
            to: m.to,
            kind: m.kind,
            payload: m.payload,
            time: m.created_at,
            unread: m.unread,
            thread: m.thread,
            acked_by: [],
          })));
        } else if (this.activeTarget.type === 'room') {
          const r = await api(this.threadURL() + '&page_size=' + this.windowSize);
          if (r.room) {
            this.activeRoomData = r.room;
          }
          this.setLoadedMessages((r.items || []).map((m) => this.threadItem(m)), r);
        } else if (this.activeTarget.type === 'peer') {
          // One DM thread per peer (op/<id>) holds both directions (§6.8):
          // the inbox only carries peer→operator, so operator sends vanished
          // on the next refresh. Same item shape as the room branch.
          const r = await api(this.threadURL() + '&page_size=' + this.windowSize);
          this.setLoadedMessages((r.items || []).map((m) => this.threadItem(m)), r);
          this.markPeerRead(this.activeTarget.id);
        } else if (this.activeTarget.type === 'broadcast') {
          // For broadcast, search messages sent to *
          const r = await api('/admin/messages?q=*&page_size=30');
          this.setLoadedMessages((r.items || []).filter((m) => m.recipient === '*').map((m) => ({
            seq: m.seq,
            id: m.id,
            from: m.sender,
            to: m.recipient,
            kind: m.kind,
            payload: m.payload,
            time: m.created_at,
            acked_by: [],
          })));
        }

        const maxSeq = this.maxMessageSeq();
        if (!silent) {
          // Entering or switching a conversation lands on the latest message
          // instantly — an IM opens at the bottom, it does not scroll there.
          this.newBelow = false;
          this.scrollToBottom(true);
        } else if (wasAtBottom) {
          this.newBelow = false;
          this.scrollToBottom(false);
        } else if (maxSeq > prevMaxSeq) {
          this.newBelow = true;
        }
      } catch (err) {
        if (!silent) toast('加载消息流失败：' + err.message, 'bad');
      } finally {
        if (!silent) this.loadingMessages = false;
      }
    },

    // 3s poll for room/DM threads: forward-only from the newest seq the client
    // already holds. The old path re-fetched the thread and re-rendered the
    // whole payload every tick.
    async pollThreadMessages() {
      const root = this.threadRoot();
      if (!root) return;
      let cursor = this.newestSeq || this.maxMessageSeq();
      if (!cursor) {
        await this.loadActiveMessages(true);
        return;
      }
      const stream = $('cmdStreamContainer');
      const wasAtBottom = !stream || this.isAtBottom(stream);
      let added = [];
      try {
        // Bounded catch-up: at most 5 * 200 new rows in one tick, the next
        // tick starts from the advanced cursor.
        for (let i = 0; i < 5; i++) {
          const r = await api(this.threadURL() + '&after_seq=' + cursor + '&page_size=200');
          if (this.threadRoot() !== root) return;
          if (r.room && this.activeTarget.type === 'room') this.activeRoomData = r.room;
          if (Number.isFinite(r.total)) this.totalMessages = r.total;
          const items = (r.items || []).map((m) => this.threadItem(m));
          const fresh = items.filter((m) => m.seq > cursor);
          if (fresh.length) {
            added = added.concat(fresh);
            this.messages = this.messages.concat(fresh);
            cursor = fresh[fresh.length - 1].seq;
            this.newestSeq = cursor;
          }
          if (!r.has_more || !fresh.length) break;
        }
      } catch (_) {
        return;
      }
      if (!added.length) return;
      if (this.activeTarget.type === 'peer') this.markPeerRead(this.activeTarget.id);
      if (wasAtBottom) {
        this.newBelow = false;
        this.scrollToBottom(false);
      } else {
        this.newBelow = true;
        this.$nextTick(() => this.measureVisibleRows());
      }
    },

    // "加载更早消息": fetch the page before the oldest loaded seq and splice it
    // in. The anchor row stays at the same viewport position; when the reader
    // was sitting at the top, the newly loaded block is shown above it.
    async loadOlderMessages() {
      if (this.loadingOlder || !this.hasOlder) return;
      const root = this.threadRoot();
      if (!root) return;
      const cursor = this.oldestSeq || (this.messages[0] && this.messages[0].seq) || 0;
      if (!cursor) return;
      const el = $('cmdStreamContainer');
      const oldScrollTop = el ? el.scrollTop : 0;
      const wasAtTop = !el || oldScrollTop <= 40;
      const anchor = el ? this.firstVisibleRow(el) : null;
      this.loadingOlder = true;
      try {
        const r = await api(this.threadURL() + '&before_seq=' + cursor + '&page_size=' + this.windowSize);
        if (this.threadRoot() !== root) return;
        const seen = new Set(this.messages.map((m) => m.seq));
        const older = (r.items || []).map((m) => this.threadItem(m)).filter((m) => !seen.has(m.seq));
        if (!older.length) {
          this.hasOlder = false;
          return;
        }
        this.messages = older.concat(this.messages);
        this.totalMessages = Number.isFinite(r.total) ? r.total : this.messages.length;
        this.hasOlder = !!r.has_more;
        this.oldestSeq = r.oldest_seq || older[0].seq;
        const idx = anchor ? this.messages.findIndex((m) => m.seq === anchor.seq) : -1;
        if (idx < 0) {
          this.windowStart = Math.max(0, this.messages.length - this.windowSize);
          return;
        }
        // Keep ~60% of the window below the anchor, so the loaded history
        // fills the space above it instead of replacing rows on screen.
        this.windowStart = Math.max(0, Math.min(
          this.messages.length - this.windowSize,
          idx - Math.floor(this.windowSize * 0.6),
        ));
        // Scroll after the row heights/spacers have been measured (two ticks:
        // the spacer style update is itself a reactive tick). The jump is
        // instant so it cannot race the CSS smooth scrolling.
        const anchorViewport = anchor.top - oldScrollTop;
        this.$nextTick(() => {
          this.measureVisibleRows();
          this.$nextTick(() => {
            if (!el) return;
            const row = this.rowForSeq(el, anchor.seq);
            if (!row) return;
            if (wasAtTop) {
              this.jumpScroll(el, row.offsetTop - el.clientHeight + row.offsetHeight);
            } else {
              this.jumpScroll(el, row.offsetTop - anchorViewport);
            }
          });
        });
      } catch (err) {
        toast('加载更早消息失败：' + err.message, 'bad');
      } finally {
        this.loadingOlder = false;
      }
    },

    rowForSeq(el, seq) {
      const rows = el.querySelectorAll('[data-vseq]');
      for (const row of rows) {
        if (Number(row.dataset.vseq) === seq) return row;
      }
      return null;
    },

    firstVisibleRow(el) {
      const rows = el.querySelectorAll('[data-vseq]');
      for (const row of rows) {
        if (row.offsetTop + row.offsetHeight > el.scrollTop + 1) {
          return { seq: Number(row.dataset.vseq), top: row.offsetTop };
        }
      }
      return null;
    },

    // Cache measured row heights. Spacer heights derive from this cache, so a
    // caller that changes the window must re-anchor itself after the tick (see
    // updateVisibleWindow / loadOlderMessages).
    measureVisibleRows() {
      const el = $('cmdStreamContainer');
      if (!el || !el.querySelectorAll) return;
      const rows = el.querySelectorAll('[data-vseq]');
      for (const row of rows) {
        const seq = Number(row.dataset.vseq);
        const h = row.offsetHeight || row.getBoundingClientRect().height;
        if (!Number.isFinite(h) || h <= 0) continue;
        if (!Number.isFinite(this.msgHeights[seq]) || Math.abs(this.msgHeights[seq] - h) > 0.5) {
          this.msgHeights[seq] = h;
        }
      }
    },

    // Programmatic window anchors must not animate: CSS scroll-behavior is
    // smooth for the user's "jump to bottom" ride, but an anchor correction
    // that animates emits intermediate scroll events and makes the window
    // chase itself. The flag swallows those synthetic scroll events.
    jumpScroll(el, value) {
      if (!el) return;
      this._vtAdjusting = true;
      clearTimeout(this._vtTimer);
      const prev = el.style.scrollBehavior;
      el.style.scrollBehavior = 'auto';
      el.scrollTop = Math.max(0, value);
      el.style.scrollBehavior = prev;
      this._vtTimer = setTimeout(() => { this._vtAdjusting = false; }, 120);
    },

    nudgeScroll(el, delta) {
      this.jumpScroll(el, el.scrollTop + delta);
    },

    updateVisibleWindow(el) {
      const n = this.messages.length;
      if (!n) return;
      const maxStart = Math.max(0, n - this.windowSize);
      if (this.isAtBottom(el)) {
        if (this.windowStart !== maxStart) this.windowStart = maxStart;
        return;
      }
      const idx = this.messageIndexAtOffset(el.scrollTop);
      const start = Math.max(0, Math.min(maxStart, idx - CMD_OVERSCAN));
      if (Math.abs(start - this.windowStart) < 2) return;
      const anchor = this.firstVisibleRow(el);
      const anchorViewport = anchor ? anchor.top - el.scrollTop : 0;
      this.windowStart = start;
      this.$nextTick(() => {
        this.measureVisibleRows();
        this.$nextTick(() => {
          if (!anchor) return;
          const row = this.rowForSeq(el, anchor.seq);
          if (row) this.jumpScroll(el, row.offsetTop - anchorViewport);
        });
      });
    },

    // 32px matches the log view's "already at the bottom" slack.
    isAtBottom(el) {
      return el.scrollHeight - el.scrollTop - el.clientHeight <= CMD_BOTTOM_SLACK;
    },

    // instant skips the container's CSS smooth scrolling: opening a thread
    // must jump to the bottom, not animate down from the top. Sending and
    // following a new message keep the smooth ride.
    scrollToBottom(instant = false) {
      const el = $('cmdStreamContainer');
      const start = Math.max(0, this.messages.length - this.windowSize);
      if (this.windowStart !== start) this.windowStart = start;
      const jump = () => {
        if (!el) return;
        if (instant) {
          this.jumpScroll(el, el.scrollHeight);
        } else {
          el.scrollTop = el.scrollHeight;
        }
      };
      if (this.$nextTick) {
        // Two ticks: row measurement changes spacer heights, so jump only
        // after that layout is committed.
        this.$nextTick(() => {
          this.measureVisibleRows();
          this.$nextTick(jump);
        });
      } else {
        jump();
      }
      this.stickToBottom = true;
      this.newBelow = false;
    },

    // The reader scrolled themselves back to the bottom: nothing is "below".
    // While they move through history, slide the render window with them.
    onStreamScroll() {
      const el = $('cmdStreamContainer');
      if (!el) return;
      if (this.isAtBottom(el)) this.newBelow = false;
      if (this._vtAdjusting) return;
      this.updateVisibleWindow(el);
    },

    // Sending message
    async sendMessage() {
      const text = this.payload.trim();
      if (!text) {
        toast('内容不能为空', 'warn');
        return;
      }
      this.sending = true;
      try {
        const to = this.activeTarget.id;
        const res = await api('/admin/messages', {
          method: 'POST',
          body: JSON.stringify({
            to,
            kind: this.kind,
            payload: text,
          }),
        });

        // Optimistically append message
        this.messages.push({
          seq: res.seq,
          id: res.id,
          from: 'operator',
          to,
          kind: this.kind,
          payload: text,
          time: Math.floor(Date.now() / 1000),
          acked_by: [],
        });
        this.totalMessages += 1;
        if (res.seq) this.newestSeq = Math.max(this.newestSeq || 0, res.seq);

        this.payload = '';
        this.scrollToBottom(false);
        toast(`指令已发送至 ${this.activeTarget.name} (seq ${res.seq})`, 'ok');
        refreshStats();
      } catch (err) {
        toast('发送失败：' + err.message, 'bad');
      } finally {
        this.sending = false;
      }
    },

    handleComposerKeydown(e) {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        this.sendMessage();
      }
    },

    insertMention(peerId) {
      const mention = `@${peerId} `;
      const textarea = $('cmdComposerTextarea');
      if (!textarea) {
        this.payload += mention;
        return;
      }
      const start = textarea.selectionStart;
      const end = textarea.selectionEnd;
      this.payload = this.payload.substring(0, start) + mention + this.payload.substring(end);
      this.$nextTick(() => {
        textarea.focus();
        textarea.selectionStart = textarea.selectionEnd = start + mention.length;
      });
    },

    async markAllInboxRead() {
      try {
        const maxSeq = this.messages.reduce((max, m) => Math.max(max, m.seq || 0), 0);
        await api('/admin/inbox/read', {
          method: 'POST',
          body: JSON.stringify({ seq: maxSeq }),
        });
        this.inboxUnread = 0;
        this.peerUnread = {};
        toast('收件箱已全部标记为已读', 'ok');
        this.loadSessions();
        refreshStats();
      } catch (err) {
        toast('标记已读失败：' + err.message, 'bad');
      }
    },

    // Modal: Create Room
    openCreateRoom() {
      this.newRoom = {
        id: '',
        name: '',
        members: [],
      };
      $('dlgCreateRoom')?.showModal();
    },

    async submitCreateRoom() {
      const rawId = this.newRoom.id.trim();
      if (!rawId) {
        toast('请输入群聊短名', 'warn');
        return;
      }
      try {
        const res = await api('/admin/rooms', {
          method: 'POST',
          body: JSON.stringify({
            id: rawId,
            name: this.newRoom.name.trim(),
            members: this.newRoom.members,
          }),
        });
        toast(`群聊 ${res.room?.id || rawId} 创建成功`, 'ok');
        $('dlgCreateRoom')?.close();
        await this.loadSessions();
        if (res.room) {
          this.selectRoom(res.room);
        }
      } catch (err) {
        toast('创建群聊失败：' + err.message, 'bad');
      }
    },

    // Modal: Room Members Management
    openRoomMembersModal() {
      if (this.activeTarget.type !== 'room') return;
      $('dlgRoomMembers')?.showModal();
    },

    async addRoomMember() {
      const peerId = this.newMemberId.trim();
      if (!peerId) {
        toast('请选择要添加的成员', 'warn');
        return;
      }
      try {
        await api(`/admin/rooms/${encodeURIComponent(this.activeTarget.id)}/members`, {
          method: 'POST',
          body: JSON.stringify({ peer_id: peerId }),
        });
        toast(`已将 ${peerId} 加入群聊`, 'ok');
        this.newMemberId = '';
        await this.loadSessions();
      } catch (err) {
        toast('添加成员失败：' + err.message, 'bad');
      }
    },

    async removeRoomMember(peerId) {
      const ok = await confirmAction({
        title: `移出群聊？`,
        body: `确定将 ${peerId} 移出当前群聊 ${this.activeTarget.id} 吗？`,
        danger: true,
        okText: '移出',
      });
      if (!ok) return;

      try {
        await api(`/admin/rooms/${encodeURIComponent(this.activeTarget.id)}/members/${encodeURIComponent(peerId)}`, {
          method: 'DELETE',
        });
        toast(`已将 ${peerId} 移出群聊`, 'ok');
        await this.loadSessions();
      } catch (err) {
        toast('移出成员失败：' + err.message, 'bad');
      }
    },

    // 解散群聊: soft delete on the server (system notice into the room first,
    // then archive). History stays readable under 已解散.
    async dissolveRoom() {
      const room = (this.rooms || []).find((r) => r.id === this.activeTarget.id) || {};
      const label = room.name ? room.name + ' (' + room.id + ')' : this.activeTarget.id;
      const ok = await confirmAction({
        title: '解散群聊「' + label + '」？',
        body: '群内会先收到一条系统通知，之后本群不再投递任何消息。历史消息仍保留在「已解散」分组里，随时可查。',
        danger: true,
        okText: '解散群聊',
      });
      if (!ok) return;
      try {
        // Template literal on purpose: TestConsoleAssetsDoNotCallMissingAdminRoutes
        // probes every /admin/... literal (${…} → x), so a typo in the path fails CI.
        await api(`/admin/rooms/${encodeURIComponent(this.activeTarget.id)}/dissolve`, {
          method: 'POST',
          body: '{}',
        });
        toast('群聊「' + label + '」已解散', 'ok');
        await this.loadSessions();
        this.selectInbox();
      } catch (err) {
        toast('解散失败：' + err.message, 'bad');
      }
    },

    // Filter helpers
    filteredRooms() {
      const q = this.searchQuery.toLowerCase().trim();
      const list = (Array.isArray(this.rooms) ? this.rooms : []).filter((r) => !r.archived);
      if (!q) return list;
      return list.filter((r) =>
        (r.id && r.id.toLowerCase().includes(q)) || (r.name && r.name.toLowerCase().includes(q))
      );
    },

    // Dissolved rooms (soft-deleted: history stays readable, nothing is
    // delivered any more). Kept in a collapsed group so the main list stays
    // about rooms you can actually talk to.
    filteredArchivedRooms() {
      const q = this.searchQuery.toLowerCase().trim();
      const list = (Array.isArray(this.rooms) ? this.rooms : []).filter((r) => r.archived);
      if (!q) return list;
      return list.filter((r) =>
        (r.id && r.id.toLowerCase().includes(q)) || (r.name && r.name.toLowerCase().includes(q))
      );
    },

    roomDissolved() {
      if (this.activeTarget.type !== 'room') return false;
      if (this.activeRoomData && this.activeRoomData.archived) return true;
      const room = (this.rooms || []).find((r) => r.id === this.activeTarget.id);
      return !!(room && room.archived);
    },

    // ---- read receipts -------------------------------------------------
    // Room receipts answer "which of the room's members have seen it", so they
    // only make sense for messages addressed to the room. The room thread also
    // carries private replies (a member answering the operator, or another
    // member): those are unreadable by the rest of the room and used to sit
    // there as "已读 1/4" forever, which read like ack laziness. For them the
    // only possible reader/acker is the addressee — and a reply to the operator
    // is acked through the inbox watermark, not per message, so it shows no
    // badge at all.
    roomMessage(m) {
      const to = m.to || '';
      return to === '' || to === this.activeTarget.id;
    },
    ackMembers(m) {
      if (!this.roomMessage(m)) return [];
      const room = (this.rooms || []).find((r) => r.id === this.activeTarget.id);
      let members = room && Array.isArray(room.members) ? room.members : null;
      if (!members && this.activeRoomData && Array.isArray(this.activeRoomData.members)) {
        members = this.activeRoomData.members.map((id) => ({ id, start_seq: 0 }));
      }
      if (!members) return [];
      // Who is asked at all depends on when they joined: a member only sees
      // messages sent after their start_seq, so counting them as "未读" for
      // older messages would be wrong. The sender obviously read their own.
      return members.filter((mb) => (mb.start_seq || 0) <= (m.seq || 0));
    },
    ackEligible(m) {
      if (!this.roomMessage(m)) {
        const to = m.to || '';
        if (!to || to === 'operator' || to === 'system') return [];
        return [to].filter((id) => id !== m.from);
      }
      return this.ackMembers(m).filter((mb) => mb.id !== m.from).map((mb) => mb.id);
    },
    ackLateSent(m) {
      if (!this.roomMessage(m)) return [];
      const room = (this.rooms || []).find((r) => r.id === this.activeTarget.id);
      const members = room && Array.isArray(room.members) ? room.members : [];
      return members.filter((mb) => (mb.start_seq || 0) > (m.seq || 0)).map((mb) => mb.id);
    },
    ackReaders(m) {
      const acked = new Set(m.acked_by || []);
      return this.ackEligible(m).filter((id) => acked.has(id));
    },
    ackPending(m) {
      const acked = new Set(m.acked_by || []);
      return this.ackEligible(m).filter((id) => !acked.has(id));
    },
    // read/total are computed server-side from the contiguous read watermark.
    // The browser must not re-derive them: doing so was how the same message
    // could read "已读 4/5" in the room and "ack: 3" in the thread browser,
    // and how the number could go down when a member was removed.
    ackCounts(m) {
      const total = Number.isFinite(m.total) ? m.total : this.ackEligible(m).length;
      const read = Number.isFinite(m.read) ? m.read : this.ackReaders(m).length;
      return { read, total };
    },
    ackLabel(m) {
      const { read, total } = this.ackCounts(m);
      if (!total) return '';
      if (read >= total) return '全部已读';
      if (read === 0) return '无人已读';
      return '已读 ' + read + '/' + total;
    },
    ackTone(m) {
      const { read, total } = this.ackCounts(m);
      if (!total) return '';
      return read >= total ? 'ok' : 'warn';
    },

    // Assistants with unread replies float to the top (stable otherwise), so a
    // reply is seen where the conversation lives, not only in the inbox.
    filteredPeers() {
      const q = this.searchQuery.toLowerCase().trim();
      let list = Array.isArray(this.peers) ? this.peers : [];
      if (q) {
        list = list.filter((p) =>
          (p.id && p.id.toLowerCase().includes(q)) || (p.display_name && p.display_name.toLowerCase().includes(q))
        );
      }
      const unread = this.peerUnread || {};
      return list
        .map((p, i) => ({ p, i, u: unread[p.id] > 0 ? 1 : 0 }))
        .sort((a, b) => b.u - a.u || a.i - b.i)
        .map((x) => x.p);
    },

    getMemberPeers() {
      if (!this.activeRoomData || !this.activeRoomData.members) return [];
      const memberIds = new Set(this.activeRoomData.members.map((m) => typeof m === 'object' ? m.id : m));
      return (this.peers || []).filter((p) => memberIds.has(p.id));
    },

    getNonMemberPeers() {
      if (!this.activeRoomData || !this.activeRoomData.members) return this.peers || [];
      const memberIds = new Set(this.activeRoomData.members.map((m) => typeof m === 'object' ? m.id : m));
      return (this.peers || []).filter((p) => !memberIds.has(p.id));
    },

    formatTime(ts) {
      return fmtTime(ts);
    },

    formatAgo(ts) {
      return ago(ts);
    },

    renderAvatar(id, cls = '') {
      return avatar(id, cls);
    },

    // ---- long-message reading ------------------------------------------
    messageKey(m) {
      return String((m && (m.seq ?? m.id)) ?? '');
    },

    needsCollapse(m) {
      return messageIsLong(m && m.payload);
    },

    isExpanded(m) {
      return !!this.expandedMsgs[this.messageKey(m)];
    },

    toggleMessage(m) {
      const key = this.messageKey(m);
      this.expandedMsgs[key] = !this.expandedMsgs[key];
    },

    renderMessage(payload) {
      return renderMessageHTML(payload);
    },

    async copyMessage(m) {
      const ok = await copyToClipboard(m && m.payload);
      toast(ok ? '已复制消息内容' : '复制失败', ok ? 'ok' : 'bad');
    },
  };
}
