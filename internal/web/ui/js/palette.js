// Global Command Palette (⌘K / Ctrl+K) for agent-relay console
import { api } from './api.js';
import { $, esc, icon, setupDialog, toast } from './utils.js';
import { routeMeta, setRoute, toggleSidebarCollapsed } from './router.js';
import { lastStats } from './auth.js';

const ROUTE_KEYWORDS = {
  work: 'work queue attention tickets todo 待办 工单 工作项 审批',
  members: 'members peers online status 成员 助手 在线 心跳',
  muse: 'muse keepalive wake cookie auth 保活 唤醒 授权',
  threads: 'threads messages search fuse approve 线程 消息 熔断 审批',
  command: 'command chat room broadcast inbox 指挥台 群聊 广播 收件箱',
  prompts: 'prompts invite onboarding reconfigure 邀请 入驻 重配',
  resources: 'resources secrets mcp skill prompt 资源 密钥',
  audit: 'audit security events 审计 安全 日志',
  logs: 'logs agent watchdog receiver 日志 排障',
  tokens: 'tokens rotate revoke key 令牌 凭证 吊销',
  system: 'system policy config update upgrade 系统 策略 更新 升级',
};

const ROUTE_ICONS = {
  work: 'inbox',
  members: 'users',
  muse: 'zap',
  threads: 'message',
  command: 'send',
  prompts: 'user-plus',
  resources: 'package',
  audit: 'scroll',
  logs: 'terminal',
  tokens: 'key',
  system: 'server',
};

function routeBadge(route, stats) {
  if (!stats) return '';
  switch (route) {
    case 'work':
      return stats.attention_tickets > 0 ? { text: `${stats.attention_tickets} 待处理`, tone: 'warn' } : '';
    case 'members':
      return Number.isFinite(stats.peers_online)
        ? { text: `${stats.peers_online}/${stats.peers_total || 0} 在线`, tone: 'ok' }
        : '';
    case 'muse':
      return stats.muse_needs_reauth > 0 ? { text: `${stats.muse_needs_reauth} 待授权`, tone: 'warn' } : '';
    case 'threads':
      return stats.pending_approvals > 0 ? { text: `${stats.pending_approvals} 待审批`, tone: 'warn' } : '';
    case 'command':
      return stats.inbox_unread > 0 ? { text: `${stats.inbox_unread} 未读`, tone: 'warn' } : '';
    case 'resources': {
      const n = (stats.resource_submissions_pending || 0) + (stats.resource_takedowns_pending || 0);
      return n > 0 ? { text: `${n} 待审核`, tone: 'warn' } : '';
    }
    case 'logs':
      return stats.agent_log_errors_24h > 0 ? { text: `${stats.agent_log_errors_24h} error`, tone: 'bad' } : '';
    default:
      return '';
  }
}

export function buildPaletteItems({
  routes = routeMeta,
  stats = null,
  peers = [],
  rooms = [],
  theme = 'dark',
  collapsed = false,
} = {}) {
  const items = [];

  for (const [id, meta] of Object.entries(routes)) {
    if (id === 'muse-chat') continue;
    items.push({
      id: `route:${id}`,
      group: '页面导航',
      title: meta.title,
      subtitle: meta.desc || '',
      icon: ROUTE_ICONS[id] || 'arrow-r',
      keywords: `${id} ${meta.title} ${ROUTE_KEYWORDS[id] || ''}`.toLowerCase(),
      badge: routeBadge(id, stats),
      kind: 'route',
      route: id,
    });
  }

  const activeRooms = (Array.isArray(rooms) ? rooms : []).filter((r) => !r.archived);
  for (const r of activeRooms) {
    const label = r.name || r.id;
    const count = Array.isArray(r.members) ? r.members.length : 0;
    items.push({
      id: `room:${r.id}`,
      group: '群聊频道',
      title: label,
      subtitle: `${r.id} · ${count} 名成员`,
      icon: 'users',
      keywords: `room group 群聊 ${r.id} ${r.name || ''}`.toLowerCase(),
      badge: '',
      kind: 'room',
      roomId: r.id,
    });
  }

  const sortedPeers = (Array.isArray(peers) ? peers : []).slice().sort((a, b) => {
    const ao = a.online ? 1 : 0;
    const bo = b.online ? 1 : 0;
    if (bo !== ao) return bo - ao;
    return String(a.id || '').localeCompare(String(b.id || ''));
  });
  for (const p of sortedPeers) {
    const type = p.agent_type || 'agent';
    items.push({
      id: `peer:${p.id}`,
      group: '成员私聊',
      title: p.id,
      subtitle: `${p.online ? '在线' : '离线'} · ${type}${p.display_name && p.display_name !== p.id ? ' · ' + p.display_name : ''}`,
      icon: 'bot',
      keywords: `peer dm chat 助手 私聊 ${p.id} ${p.display_name || ''} ${type} ${p.admin_note || ''}`.toLowerCase(),
      badge: p.online ? { text: '在线', tone: 'ok' } : '',
      kind: 'peer',
      peerId: p.id,
    });
  }

  items.push(
    {
      id: 'act:theme',
      group: '快捷操作',
      title: theme === 'dark' ? '切换为浅色主题' : '切换为深色主题',
      subtitle: '同步保存偏好到当前浏览器',
      icon: theme === 'dark' ? 'sun' : 'moon',
      keywords: 'theme dark light 主题 深色 浅色 外观',
      badge: '',
      kind: 'action',
      action: 'toggle-theme',
    },
    {
      id: 'act:sidebar',
      group: '快捷操作',
      title: collapsed ? '展开左侧导航栏' : '折叠左侧导航栏（窄轨模式）',
      subtitle: '腾出更多主工作区空间',
      icon: 'panel-left',
      keywords: 'sidebar collapse rail 侧边栏 折叠 窄轨',
      badge: '',
      kind: 'action',
      action: 'toggle-sidebar',
    },
    {
      id: 'act:create-room',
      group: '快捷操作',
      title: '新建协作群聊',
      subtitle: '跳转到指挥台并打开建群弹窗',
      icon: 'plus',
      keywords: 'create room group 新建 群聊',
      badge: '',
      kind: 'action',
      action: 'create-room',
    },
    {
      id: 'act:rotate-token',
      group: '快捷操作',
      title: '签发新 Token（轮换）',
      subtitle: '前往 Token 管理并签发个人凭证',
      icon: 'key',
      keywords: 'rotate token issue key 签发 轮换',
      badge: '',
      kind: 'action',
      action: 'rotate-token',
    },
    {
      id: 'act:muse-auth',
      group: '快捷操作',
      title: '授权 Muse 保活账号',
      subtitle: '前往 Muse 保活并打开 Cookie 授权弹窗',
      icon: 'zap',
      keywords: 'muse authorize cookie 保活 授权',
      badge: '',
      kind: 'action',
      action: 'muse-auth',
    },
  );

  return items;
}

export function filterPaletteItems(items, query = '') {
  const q = String(query || '').trim().toLowerCase();
  if (!q) {
    // Keep initial palette clean by capping room/peer lists when query is empty.
    const counts = { '群聊频道': 0, '成员私聊': 0 };
    return items.filter((item) => {
      if (item.group === '群聊频道') return ++counts['群聊频道'] <= 5;
      if (item.group === '成员私聊') return ++counts['成员私聊'] <= 6;
      return true;
    });
  }

  const tokens = q.split(/\s+/).filter(Boolean);
  const scored = [];
  for (let i = 0; i < items.length; i++) {
    const it = items[i];
    const title = (it.title || '').toLowerCase();
    const sub = (it.subtitle || '').toLowerCase();
    const kw = (it.keywords || '').toLowerCase();
    const hay = `${title} ${sub} ${kw}`;
    if (!tokens.every((t) => hay.includes(t))) continue;

    let score = 0;
    if (title === q) score += 100;
    else if (title.startsWith(q)) score += 60;
    else if (title.includes(q)) score += 30;
    if (it.group === '页面导航') score += 10;
    if (it.badge && it.badge.tone === 'ok') score += 4;
    scored.push({ it, score, i });
  }
  scored.sort((a, b) => b.score - a.score || a.i - b.i);
  return scored.map((x) => x.it);
}

const cache = {
  peers: [],
  rooms: [],
  fetchedAt: 0,
  fetching: null,
};

let visibleItems = [];
let activeIdx = 0;
let lastFocusedEl = null;

async function ensureDirectoryData(onReady) {
  const now = Date.now();
  if (now - cache.fetchedAt < 30_000 && cache.fetchedAt > 0) return;
  if (cache.fetching) return;
  cache.fetching = Promise.allSettled([
    api('/admin/peers?page_size=200'),
    api('/admin/rooms'),
  ]).then(([peersRes, roomsRes]) => {
    if (peersRes.status === 'fulfilled' && Array.isArray(peersRes.value?.peers)) {
      cache.peers = peersRes.value.peers;
    }
    if (roomsRes.status === 'fulfilled' && Array.isArray(roomsRes.value?.rooms)) {
      cache.rooms = roomsRes.value.rooms;
    }
    cache.fetchedAt = Date.now();
  }).finally(() => {
    cache.fetching = null;
    if ($('cmdPalette')?.open) onReady();
  });
}

function renderList() {
  const list = $('cmdPaletteList');
  const input = $('cmdPaletteInput');
  if (!list) return;

  const theme = document.documentElement.dataset.theme || 'dark';
  const collapsed = $('app')?.classList.contains('sb-collapsed') || false;
  const all = buildPaletteItems({
    routes: routeMeta,
    stats: lastStats,
    peers: cache.peers,
    rooms: cache.rooms,
    theme,
    collapsed,
  });
  visibleItems = filterPaletteItems(all, input?.value || '');
  if (activeIdx >= visibleItems.length) activeIdx = Math.max(0, visibleItems.length - 1);

  if (!visibleItems.length) {
    list.innerHTML = `<div class="palette-empty">未找到匹配的页面、成员或操作</div>`;
    return;
  }

  let currentGroup = '';
  const html = [];
  visibleItems.forEach((item, idx) => {
    if (item.group !== currentGroup) {
      currentGroup = item.group;
      html.push(`<div class="palette-group">${esc(currentGroup)}</div>`);
    }
    const badge = item.badge
      ? `<span class="badge ${esc(item.badge.tone || '')}">${esc(item.badge.text)}</span>`
      : '';
    html.push(`<button type="button" class="palette-item${idx === activeIdx ? ' is-active' : ''}" data-idx="${idx}" role="option" aria-selected="${idx === activeIdx ? 'true' : 'false'}">
      ${icon(item.icon || 'arrow-r')}
      <span class="palette-item-main">
        <span class="palette-item-title">${esc(item.title)}</span>
        ${item.subtitle ? `<span class="palette-item-sub">${esc(item.subtitle)}</span>` : ''}
      </span>
      <span class="palette-item-meta">${badge}</span>
    </button>`);
  });
  list.innerHTML = html.join('');

  list.querySelectorAll('.palette-item').forEach((btn) => {
    btn.onclick = () => executeItem(visibleItems[Number(btn.dataset.idx)]);
    btn.onmousemove = () => {
      const idx = Number(btn.dataset.idx);
      if (idx === activeIdx) return;
      activeIdx = idx;
      updateActiveRow();
    };
  });
}

function updateActiveRow() {
  const list = $('cmdPaletteList');
  if (!list) return;
  list.querySelectorAll('.palette-item').forEach((btn) => {
    const on = Number(btn.dataset.idx) === activeIdx;
    btn.classList.toggle('is-active', on);
    btn.setAttribute('aria-selected', on ? 'true' : 'false');
    if (on) btn.scrollIntoView({ block: 'nearest' });
  });
}

function closePalette() {
  const dlg = $('cmdPalette');
  if (dlg?.open) dlg.close();
}

function executeItem(item) {
  if (!item) return;
  closePalette();
  switch (item.kind) {
    case 'route':
      setRoute(item.route);
      break;
    case 'room':
      setRoute(`command?room=${encodeURIComponent(item.roomId)}`);
      break;
    case 'peer':
      setRoute(`command?peer=${encodeURIComponent(item.peerId)}`);
      break;
    case 'action':
      if (item.action === 'toggle-theme') {
        $('themeBtn')?.click();
        toast(`已切换至${document.documentElement.dataset.theme === 'dark' ? '深色' : '浅色'}主题`, 'info', 1800);
      } else if (item.action === 'toggle-sidebar') {
        toggleSidebarCollapsed();
      } else if (item.action === 'create-room') {
        setRoute('command');
        setTimeout(() => $('dlgCreateRoom')?.showModal(), 40);
      } else if (item.action === 'rotate-token') {
        setRoute('tokens');
        setTimeout(() => $('openRotate')?.click(), 40);
      } else if (item.action === 'muse-auth') {
        setRoute('muse');
        setTimeout(() => $('museAuthorize')?.click(), 40);
      }
      break;
  }
}

export function openPalette() {
  const dlg = $('cmdPalette');
  if (!dlg || $('app')?.hidden) return;
  if (dlg.open) {
    dlg.close();
    return;
  }
  lastFocusedEl = document.activeElement;
  const input = $('cmdPaletteInput');
  if (input) input.value = '';
  activeIdx = 0;
  renderList();
  dlg.showModal();
  input?.focus();
  ensureDirectoryData(renderList);
}

export function initPalette() {
  const dlg = $('cmdPalette');
  if (!dlg) return;
  setupDialog(dlg);

  dlg.addEventListener('close', () => {
    if (lastFocusedEl && typeof lastFocusedEl.focus === 'function' && document.contains(lastFocusedEl)) {
      lastFocusedEl.focus();
    }
    lastFocusedEl = null;
  });

  const trigger = $('cmdPaletteBtn');
  if (trigger) trigger.onclick = openPalette;

  const input = $('cmdPaletteInput');
  if (input) {
    input.oninput = () => {
      activeIdx = 0;
      renderList();
    };
    input.onkeydown = (e) => {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        if (visibleItems.length) {
          activeIdx = (activeIdx + 1) % visibleItems.length;
          updateActiveRow();
        }
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        if (visibleItems.length) {
          activeIdx = (activeIdx - 1 + visibleItems.length) % visibleItems.length;
          updateActiveRow();
        }
      } else if (e.key === 'Enter') {
        if (visibleItems[activeIdx]) {
          e.preventDefault();
          executeItem(visibleItems[activeIdx]);
        }
      }
    };
  }

  document.addEventListener('keydown', (e) => {
    if (!(e.metaKey || e.ctrlKey) || e.key.toLowerCase() !== 'k') return;
    if ($('app')?.hidden) return;
    const otherOpen = document.querySelector('dialog[open]:not(#cmdPalette)');
    if (otherOpen) return;
    e.preventDefault();
    openPalette();
  });
}
