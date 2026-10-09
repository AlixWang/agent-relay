// Command & Group Chat Console module (Alpine.js component)
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, avatar, toast, confirmAction, setupDialog } from './utils.js';
import { currentRoute, onRoute } from './router.js';
import { refreshStats } from './auth.js';

let commandPollTimer = null;

export function commandApp() {
  return {
    // Session state
    searchQuery: '',
    inboxUnread: 0,
    inboxItems: [],
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
    
    // Message stream
    messages: [],
    loadingMessages: false,
    
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
      // Setup modals
      setupDialog($('dlgCreateRoom'));
      setupDialog($('dlgRoomMembers'));

      // Check if URL specifies a target (e.g. #command?room=grp_ops)
      this.handleHashParams();

      // Listen to route enter & leave
      onRoute('command', () => {
        this.handleHashParams();
        this.loadSessions();
        this.startPolling();
      }, () => {
        this.stopPolling();
      });

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
        const [roomsRes, inboxRes] = await Promise.all([
          api('/admin/rooms'),
          api('/admin/inbox?page_size=10'),
        ]);
        if (roomsRes.rooms) this.rooms = roomsRes.rooms;
        if (inboxRes) {
          this.inboxUnread = inboxRes.unread || 0;
          this.inboxItems = inboxRes.items || [];
        }
      } catch (_) {}

      // 2. Silent message stream refresh
      await this.loadActiveMessages(true);
    },

    async loadSessions() {
      try {
        const [roomsRes, inboxRes, peersRes] = await Promise.all([
          api('/admin/rooms'),
          api('/admin/inbox?page_size=30'),
          api('/admin/peers?page_size=100'),
        ]);
        this.rooms = roomsRes.rooms || [];
        this.inboxUnread = inboxRes.unread || 0;
        this.inboxItems = inboxRes.items || [];
        this.peers = peersRes.peers || [];

        // If target is a room, match room name from list
        if (this.activeTarget.type === 'room') {
          const found = this.rooms.find((r) => r.id === this.activeTarget.id);
          if (found) {
            this.activeTarget.name = found.name ? `${found.name} (${found.id})` : found.id;
            this.activeTarget.subtitle = `${(found.members || []).length} 名成员`;
          }
        }
        await this.loadActiveMessages(false);
      } catch (err) {
        toast('加载指挥台会话失败：' + err.message, 'bad');
      }
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
      this.loadActiveMessages(false);
    },

    selectRoom(room) {
      this.activeTarget = {
        type: 'room',
        id: room.id,
        name: room.name ? `${room.name} (${room.id})` : room.id,
        subtitle: `${(room.members || []).length} 名成员 · 群聊频道`,
      };
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
      this.loadActiveMessages(false);
    },

    // Message loading
    async loadActiveMessages(silent = false) {
      if (!silent) this.loadingMessages = true;
      try {
        if (this.activeTarget.type === 'inbox') {
          const r = await api('/admin/inbox?page_size=50');
          this.inboxUnread = r.unread || 0;
          this.messages = (r.items || []).map((m) => ({
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
          }));
        } else if (this.activeTarget.type === 'room') {
          const r = await api('/admin/messages?thread=' + encodeURIComponent(this.activeTarget.id));
          if (r.room) {
            this.activeRoomData = r.room;
          }
          this.messages = (r.items || []).map((m) => ({
            seq: m.seq,
            id: m.id,
            from: m.sender,
            to: m.recipient,
            kind: m.kind,
            payload: m.payload,
            time: m.created_at,
            acked_by: m.acked_by || [],
            approval_state: m.approval_state,
          }));
        } else if (this.activeTarget.type === 'peer') {
          // Fetch inbox messages from this peer and search sent messages
          const r = await api('/admin/inbox?page_size=50');
          const related = (r.items || []).filter((m) => m.from === this.activeTarget.id);
          this.messages = related.map((m) => ({
            seq: m.seq,
            id: m.id,
            from: m.from,
            to: m.to,
            kind: m.kind,
            payload: m.payload,
            time: m.created_at,
            acked_by: [],
          }));
        } else if (this.activeTarget.type === 'broadcast') {
          // For broadcast, search messages sent to *
          const r = await api('/admin/messages?q=*&page_size=30');
          this.messages = (r.items || []).filter((m) => m.recipient === '*').map((m) => ({
            seq: m.seq,
            id: m.id,
            from: m.sender,
            to: m.recipient,
            kind: m.kind,
            payload: m.payload,
            time: m.created_at,
            acked_by: [],
          }));
        }

        if (!silent) {
          this.$nextTick(() => this.scrollToBottom());
        }
      } catch (err) {
        if (!silent) toast('加载消息流失败：' + err.message, 'bad');
      } finally {
        if (!silent) this.loadingMessages = false;
      }
    },

    scrollToBottom() {
      const el = $('cmdStreamContainer');
      if (el) {
        el.scrollTop = el.scrollHeight;
      }
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

        this.payload = '';
        this.$nextTick(() => this.scrollToBottom());
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

    // Filter helpers
    filteredRooms() {
      const q = this.searchQuery.toLowerCase().trim();
      if (!q) return this.rooms;
      return this.rooms.filter((r) =>
        r.id.toLowerCase().includes(q) || (r.name && r.name.toLowerCase().includes(q))
      );
    },

    filteredPeers() {
      const q = this.searchQuery.toLowerCase().trim();
      if (!q) return this.peers;
      return this.peers.filter((p) =>
        p.id.toLowerCase().includes(q) || (p.display_name && p.display_name.toLowerCase().includes(q))
      );
    },

    getMemberPeers() {
      if (!this.activeRoomData || !this.activeRoomData.members) return [];
      const memberIds = new Set(this.activeRoomData.members);
      return this.peers.filter((p) => memberIds.has(p.id));
    },

    getNonMemberPeers() {
      if (!this.activeRoomData || !this.activeRoomData.members) return this.peers;
      const memberIds = new Set(this.activeRoomData.members);
      return this.peers.filter((p) => !memberIds.has(p.id));
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
  };
}
