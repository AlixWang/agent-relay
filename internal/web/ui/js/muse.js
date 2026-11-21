// Muse 保活 / 账号授权 tab module
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, avatar, toast, confirmAction, setupDialog } from './utils.js';
import { currentRoute } from './router.js';
import { refreshStats } from './auth.js';

export const museState = {
  accounts: [],
  facets: {},
  status: null,
  peers: [],
  loaded: false,
  error: '',
  filter: '',
  q: '',
  timer: null,
  dialogMode: 'create',
  editAccount: null,
  onChanged: null,
  chatStatus: null,
  chatThread: null,
  chatPeer: '',
  chatSession: '',
  chatJob: null,
  chatTimer: null,
  chatLoading: false,
  chatError: '',
  chatLoadToken: 0,
  chatPollToken: 0,
  chatSending: false,
  // Incremental-render bookkeeping for #mchatStream: the markup that is
  // currently on screen and the job ids it already showed. The poll tick runs
  // once a second while a reply is pending, so re-assigning innerHTML every
  // tick replayed every card's entry animation (a visible blink) and dropped
  // text selection / scroll anchoring.
  chatSig: '',
  chatSeen: null,
};

const MUSE_ACCOUNTS = '/admin/muse/accounts';
const MUSE_KEEPALIVE_ALL = '/admin/muse/keepalive';
const MUSE_CHAT = '/admin/muse/chat';
const MUSE_CHAT_SETTINGS = '/admin/muse/chat/settings';

export const museChatURL = (id, action = '') =>
  MUSE_CHAT + '/' + encodeURIComponent(id) + (action ? '/' + action : '');

export function museChatContextMatches(snapshot, state = museState) {
  return !!snapshot && snapshot.loadToken === state.chatLoadToken &&
    snapshot.peer === state.chatPeer && snapshot.session === state.chatSession;
}

const CHAT_ACTIVE = new Set(['preparing', 'sending', 'confirming', 'waiting']);
const CHAT_TEXT_MAX = 4000;
// The panel preselects the server ceiling: Muse agent turns routinely run
// past three minutes, and a timeout is terminal (no late-reply recovery), so
// the default wait should be the longest the server allows.
const CHAT_BUDGET_DEFAULT = 300;
const CHAT_BUDGET_MIN = 30;
const CHAT_BUDGET_MAX = 300;
const CHAT_SESSION_RE = /^[A-Za-z0-9._:-]{1,128}$/;

export function museChatReady() {
  const s = museState.chatStatus;
  return !!(s && s.deploy_enabled && s.runtime_enabled);
}

export function museChatStatusLabel(status) {
  return ({
    preparing: '准备中', sending: '发送中', confirming: '确认投递', waiting: '等待回复',
    replied: '已回复', timeout: '等待超时', delivery_unknown: '投递未知',
    reply_unknown: '回复未知', failed: '失败', cancelled: '已取消',
  })[status] || status || '—';
}

// Parameterised admin URL. Kept as concatenation so the console's route
// contract test sees a real prefix (/admin/muse/accounts) rather than a
// template literal it would probe with a bogus id.
export const museURL = (id, action = '') =>
  MUSE_ACCOUNTS + '/' + encodeURIComponent(id) + (action ? '/' + action : '');

// Session health is deliberately narrower than auth_state: it also requires the
// last keepalive to have succeeded without a session-scoped error. "正常" means
// the session itself is healthy; wake health is shown separately.
export function museSessionOK(a) {
  if (!a) return false;
  return a.enabled !== false && a.auth_state === 'ok' &&
    !!a.last_attempt_ok && !a.last_error;
}

// A failed wake stays unconfirmed even if the peer heartbeat later returns.
// wake_recovered is only meaningful when the wake itself was verified (Noise
// handshake + one read-only RPC; legacy rows used vm_state=RUNNING).
export function museWakeUnconfirmed(a) {
  if (!a) return false;
  if (a.wake_unconfirmed || a.wake_state === 'unconfirmed' || a.last_wake_error) return true;
  if (a.wake_state === 'noise_confirmed' && a.last_wake_ok === false) return true;
  // Older list payloads lack wake_state; only then fall back to the recorded
  // wake booleans. A newer keepalive may clear a failed manual wake without
  // rewriting last_wake_at, so wake_state is authoritative when present.
  if (a.wake_state) return false;
  return a.last_wake_at > 0 && a.last_wake_ok === false;
}

export function museWakeTone(a) {
  if (museWakeUnconfirmed(a)) return 'warn';
  if (a && (a.wake_recovered || a.wake_state === 'recovered' || a.wake_state === 'noise_confirmed')) return 'ok';
  if (a && (a.wake_pending || a.wake_state === 'pending' || a.wake_state === 'running')) return 'info';
  return 'warn';
}

export function museWakeLabel(a) {
  if (!a) return '—';
  if (museWakeUnconfirmed(a)) return '唤醒未确认';
  if (a.wake_recovered || a.wake_state === 'recovered') return '已上线';
  if (a.wake_state === 'noise_confirmed') return 'Noise 已验证';
  if (a.wake_pending || a.wake_state === 'pending') return '唤醒中';
  if (a.wake_state === 'running') return '已唤醒';
  return '未上线';
}

export function museAuthTone(a) {
  if (!a) return 'muted';
  if (!a.enabled) return 'muted';
  if (a.auth_state === 'needs_reauth') return 'bad';
  if (museSessionOK(a)) return museWakeUnconfirmed(a) ? 'warn' : 'ok';
  return 'warn';
}

export function museAuthLabel(a) {
  if (!a) return '—';
  if (!a.enabled) return '已停用';
  if (a.auth_state === 'needs_reauth') return '待重新授权';
  if (a.auth_state === 'unverified') return '未验证';
  if (museSessionOK(a)) return museWakeUnconfirmed(a) ? '唤醒未确认' : '正常';
  if (a.auth_state === 'ok' && a.last_attempt_ok) return '有告警';
  return '最近检查失败';
}

export function museExpiryText(expiresAt, now = Math.floor(Date.now() / 1000)) {
  if (!expiresAt) return '未知';
  const d = expiresAt - now;
  if (d <= 0) return '已过期';
  if (d < 3600) return Math.ceil(d / 60) + ' 分钟后';
  if (d < 48 * 3600) return Math.ceil(d / 3600) + ' 小时后';
  return Math.floor(d / 86400) + ' 天后';
}

export function museExpiryTone(expiresAt, now = Math.floor(Date.now() / 1000)) {
  if (!expiresAt) return 'muted';
  const d = expiresAt - now;
  if (d <= 0) return 'bad';
  if (d < 48 * 3600) return 'warn';
  return 'ok';
}

export function museVMTone(vm) {
  if (vm === 'RUNNING') return 'ok';
  if (vm === 'DISABLED' || !vm || vm === 'UNKNOWN') return 'muted';
  return 'warn';
}

// An empty/UNKNOWN vm_state is not a confirmed sandbox state: say so in words
// instead of showing a bare "UNKNOWN" or an em dash. The tooltip keeps the raw
// value and the confirmation time so a stale label is auditable.
export function museVMText(a) {
  const vm = a && a.vm_state;
  return !vm || vm === 'UNKNOWN' ? '未知（休眠/未分配）' : vm;
}

export function museVMTitle(a) {
  const raw = (a && a.vm_state) || '(空)';
  const at = a && a.vm_state_at
    ? `确认于 ${fmtTime(a.vm_state_at)}（${ago(a.vm_state_at)}）`
    : '尚无确认时间';
  return `原始值: ${raw} · ${at} · 仅展示，不参与唤醒判定`;
}

const toneClass = (tone) => (tone && tone !== 'muted' ? ' ' + tone : '');

const authBadge = (a) => {
  const wakeNote = a.last_wake_error
    ? '唤醒: ' + a.last_wake_error
    : (museWakeUnconfirmed(a) ? '唤醒未确认' : '');
  const tip = [
    a.auth_state,
    a.vm_state,
    a.last_error ? '会话: ' + a.last_error : '',
    wakeNote,
  ].filter(Boolean).join(' · ');
  return `<span class="badge${toneClass(museAuthTone(a))}" title="${esc(tip)}">${esc(museAuthLabel(a))}</span>`;
};

const vmBadge = (a) => `<span class="badge${toneClass(museVMTone(a.vm_state))}" title="${esc(museVMTitle(a))}">${esc(museVMText(a))}</span>`;

const wakeCell = (a) => {
  const unconfirmed = museWakeUnconfirmed(a);
  if (!a.last_wake_at && !unconfirmed) return '<span class="muted xs">—</span>';
  const tone = museWakeTone(a);
  const tip = a.last_wake_error || (unconfirmed ? '唤醒后尚未确认 VM 可连接' : '');
  const when = a.last_wake_at
    ? `<small class="muted xs" title="${esc(fmtTime(a.last_wake_at))}">${esc(ago(a.last_wake_at))}</small>`
    : '';
  return `<div class="ver-stack"><span class="badge ${tone}"${tip ? ` title="${esc(tip)}"` : ''}>${esc(museWakeLabel(a))}</span>${when}</div>`;
};

const memberCell = (a) => `<div class="who">
  ${avatar(a.peer_id)}
  <div class="who-text">
    <b>${esc(a.peer_id)}</b>
    <small>${a.peer_online ? '在线' : '离线'}${a.peer_status ? ' · ' + esc(a.peer_status) : ''}</small>
  </div>
</div>`;

const accountCell = (a) => `<div class="who-text">
  <b>${esc(a.label || a.peer_id)}</b>
  <small>${(a.cookie_names || []).length} cookies · ${a.essential_ok ? '核心齐全' : '缺核心'}</small>
</div>`;

const rowActions = (a) => {
  const reauthTone = a.auth_state === 'needs_reauth' ? ' warn' : '';
  const chatReady = museChatReady();
  const chatTip = !chatReady ? '直接对话未启用（部署开关或运行时开关关闭）'
    : (a.chat_enabled ? '与成员 Muse agent 对话' : '开通直接对话并进入');
  const chatBtn = `<button class="btn-icon${a.chat_enabled ? '' : ' warn'}" data-act="chat" data-id="${esc(a.id)}" data-tip="${esc(chatTip)}" aria-label="${esc(chatTip)}" ${chatReady ? '' : 'disabled'}>${icon('message')}</button>`;
  return `<div class="row-btns tight">
    ${chatBtn}
    <button class="btn-icon" data-act="keepalive" data-id="${esc(a.id)}" data-tip="立即保活" aria-label="立即保活">${icon('refresh')}</button>
    <button class="btn-icon" data-act="wake" data-id="${esc(a.id)}" data-tip="唤醒 Muse 沙盒" aria-label="唤醒 Muse 沙盒">${icon('zap')}</button>
    <button class="btn-icon" data-act="logs" data-id="${esc(a.id)}" data-peer="${esc(a.peer_id)}" data-tip="查看 Agent 日志" aria-label="查看 Agent 日志">${icon('terminal')}</button>
    <button class="btn-icon${reauthTone}" data-act="reauth" data-id="${esc(a.id)}" data-tip="重新授权 / 更新 Cookie" aria-label="重新授权">${icon('key')}</button>
    <button class="btn-icon${a.enabled ? '' : ' ok'}" data-act="toggle" data-id="${esc(a.id)}" data-tip="${a.enabled ? '停用后台保活' : '启用后台保活'}" aria-label="${a.enabled ? '停用后台保活' : '启用后台保活'}">${a.enabled ? icon('pause') : icon('play')}</button>
    <button class="btn-icon danger" data-act="delete" data-id="${esc(a.id)}" data-tip="删除授权" aria-label="删除授权">${icon('trash')}</button>
  </div>`;
};

function renderStatus() {
  const all = $('museKeepaliveAll');
  if (all) all.disabled = !!(museState.status && museState.status.running);
  const el = $('museStatus');
  if (!el) return;
  const span = el.querySelector('span');
  if (!span) return;
  if (museState.error && !museState.loaded) {
    span.textContent = museState.error;
    return;
  }
  const s = museState.status || {};
  let text = '尚未运行';
  if (s.running) {
    text = '后台保活进行中…';
  } else if (s.last_result) {
    const r = s.last_result;
    text = `上次保活 ${ago(r.started_at)}：成功 ${r.ok ?? 0} / 失败 ${r.failed ?? 0} / 跳过 ${r.skipped ?? 0}`;
    if (r.needs_reauth) text += ` · ${r.needs_reauth} 个待重新授权`;
  }
  if (s.loop_enabled === false) text += ' · 后台循环已关闭（手动可用）';
  span.textContent = text;
}

function renderEmpty(tbody, message, withAction) {
  tbody.innerHTML = `<tr><td colspan="9" class="empty-td"><div class="empty">${icon('zap')}<p>${esc(message)}</p>` +
    (withAction ? '<button class="btn btn-primary btn-sm" data-act="authorize">授权第一个账号</button>' : '') +
    '</div></td></tr>';
  const btn = tbody.querySelector('[data-act="authorize"]');
  if (btn) btn.onclick = () => openMuseAuth('', refreshMuse);
}

function renderError(tbody, message) {
  tbody.innerHTML = `<tr><td colspan="9" class="empty-td"><div class="empty bad">${icon('err')}<p>${esc(message)}</p></div></td></tr>`;
}

function filteredAccounts() {
  const q = museState.q.toLowerCase();
  return museState.accounts.filter((a) => {
    if (museState.filter === 'ok' && !(museSessionOK(a) && !museWakeUnconfirmed(a))) return false;
    if (museState.filter === 'wake_unconfirmed' && !(museSessionOK(a) && museWakeUnconfirmed(a))) return false;
    if (museState.filter === 'needs_reauth' && a.auth_state !== 'needs_reauth') return false;
    if (museState.filter === 'offline' && a.peer_online) return false;
    if (q) {
      const hay = `${a.peer_id} ${a.label || ''} ${a.last_error || ''} ${a.last_wake_error || ''}`.toLowerCase();
      if (!hay.includes(q)) return false;
    }
    return true;
  });
}

function renderMuse() {
  const table = $('museTable');
  if (!table) return;
  const tbody = table.querySelector('tbody');
  if (!tbody) return;

  const f = museState.facets || {};
  const accounts = museState.accounts || [];
  const total = f.total ?? accounts.length;
  const set = (id, v) => { const el = $(id); if (el) el.textContent = v; };
  set('museTotal', total);
  set('museOK', f.ok ?? 0);
  set('museReauth', f.needs_reauth ?? 0);
  set('museAuto', f.auto_wake ?? 0);
  set('museFcAll', total);
  set('museFcOK', f.ok ?? 0);
  set('museFcWake', f.wake_unconfirmed ?? 0);
  set('museFcReauth', f.needs_reauth ?? 0);
  set('museFcOffline', f.offline ?? 0);
  const chatRuntime = $('museChatRuntime');
  if (chatRuntime) {
    const cs = museState.chatStatus || {};
    chatRuntime.checked = !!cs.runtime_enabled;
    chatRuntime.disabled = !cs.deploy_enabled;
    const label = chatRuntime.closest('label');
    if (label) label.title = cs.deploy_enabled
      ? '直接对话运行时开关（关闭会停止在途任务，不发 chat.cancel）'
      : '直接对话未部署：需在 config 中设置 muse_direct_chat_enabled=true 并重启';
  }
  const nav = $('navMuse');
  if (nav) {
    const needs = f.needs_reauth ?? 0;
    nav.textContent = needs > 0 ? needs : '';
    nav.hidden = needs === 0;
  }

  if (museState.error && !museState.loaded) {
    renderError(tbody, museState.error);
    renderStatus();
    return;
  }
  if (!accounts.length) {
    renderEmpty(tbody, '还没有授权 Muse 账号。授权后后台会续期会话并在沙盒休眠时唤醒它。', true);
    renderStatus();
    return;
  }
  const rows = filteredAccounts();
  if (!rows.length) {
    renderEmpty(tbody, '没有符合筛选条件的账号。', false);
    renderStatus();
    return;
  }
  const now = Math.floor(Date.now() / 1000);
  tbody.innerHTML = rows.map((a) => `<tr>
    <td>${memberCell(a)}</td>
    <td>${accountCell(a)}</td>
    <td>${authBadge(a)}</td>
    <td><div class="ver-stack">
      <span class="badge${toneClass(museExpiryTone(a.expires_at, now))}">${esc(museExpiryText(a.expires_at, now))}</span>
      <small class="muted xs" title="${esc(fmtTime(a.expires_at))}">${a.expires_at ? esc(fmtTime(a.expires_at)) : '未确认'}</small>
    </div></td>
    <td>${vmBadge(a)}</td>
    <td><span title="${esc(fmtTime(a.last_keepalive_at))}">${esc(ago(a.last_keepalive_at))}</span></td>
    <td>${wakeCell(a)}</td>
    <td><label class="switch" title="离线时自动唤醒"><input type="checkbox" data-act="auto" data-id="${esc(a.id)}" ${a.auto_wake ? 'checked' : ''}><span></span></label></td>
    <td class="td-actions">${rowActions(a)}</td>
  </tr>`).join('');

  tbody.querySelectorAll('button[data-act]').forEach((btn) => {
    btn.onclick = async (e) => {
      e.stopPropagation();
      const a = museState.accounts.find((x) => x.id === btn.dataset.id);
      if (!a) return;
      switch (btn.dataset.act) {
        case 'logs':
          location.hash = '#logs?peer=' + encodeURIComponent(btn.dataset.peer || '');
          break;
        case 'keepalive':
          await keepaliveAccount(a);
          break;
        case 'wake':
          await wakeMuseAccount(a.id);
          break;
        case 'reauth':
          await openMuseReauth(a, refreshMuse);
          break;
        case 'toggle':
          await toggleEnabled(a);
          break;
        case 'delete':
          await deleteAccount(a);
          break;
        case 'chat':
          await enterMuseChat(a);
          break;
      }
    };
  });
  tbody.querySelectorAll('input[data-act="auto"]').forEach((box) => {
    box.onchange = async () => {
      const a = museState.accounts.find((x) => x.id === box.dataset.id);
      if (!a) return;
      const want = box.checked;
      try {
        await api(museURL(a.id), { method: 'PATCH', body: JSON.stringify({ auto_wake: want }) });
        a.auto_wake = want;
        toast(`已${want ? '开启' : '关闭'} ${a.peer_id} 的自动唤醒`, 'ok');
        refreshStats();
      } catch (err) {
        box.checked = !want;
        toast('更新失败：' + err.message, 'bad');
      }
    };
  });
  renderStatus();
}

export async function refreshMuse() {
  const table = $('museTable');
  if (!table) return false;
  const tbody = table.querySelector('tbody');
  if (tbody && !museState.loaded) {
    tbody.innerHTML = `<tr class="skel"><td colspan="9"><i></i></td></tr>`;
  }
  try {
    const r = await api(MUSE_ACCOUNTS);
    museState.accounts = r.accounts || [];
    museState.facets = r.facets || {};
    museState.status = r.status || null;
    museState.chatStatus = r.chat_status || null;
    museState.loaded = true;
    museState.error = '';
    renderMuse();
    if (currentRoute === 'muse') startMuseTimer();
    return true;
  } catch (err) {
    if (err.status === 401) {
      stopMuseTimer();
      return false;
    }
    museState.error = err.status === 503
      ? 'Muse 集成未启用：检查服务端 muse-cookie.key 与配置。'
      : err.message;
    if (!museState.loaded) renderMuse();
    else renderStatus();
    if (currentRoute === 'muse') startMuseTimer();
    return false;
  }
}

async function ensureAccounts() {
  if (museState.loaded) return;
  const ok = await refreshMuse();
  if (!ok || !museState.loaded) throw new Error(museState.error || '加载失败');
}

async function ensurePeers() {
  if (museState.peers.length) return;
  const r = await api('/admin/peers?page_size=200');
  museState.peers = r.peers || [];
}

function showDlgError(msg) {
  const el = $('museDlgErr');
  if (!el) return;
  el.textContent = msg;
  el.hidden = false;
}

function clearDlgError() {
  const el = $('museDlgErr');
  if (el) {
    el.hidden = true;
    el.textContent = '';
  }
}

function fillDialog(mode, account, options, selected, onChanged) {
  museState.dialogMode = mode;
  museState.editAccount = account || null;
  museState.onChanged = onChanged || null;
  clearDlgError();

  const peer = $('musePeer');
  if (peer) {
    peer.innerHTML = options.map((p) =>
      `<option value="${esc(p.id)}">${esc(p.display_name || p.id)}${p.admin_note ? ' · ' + esc(p.admin_note) : ''}</option>`
    ).join('');
    peer.value = selected;
    peer.disabled = mode === 'reauth';
  }
  const label = $('museLabel');
  if (label) {
    const chosen = options.find((p) => p.id === selected);
    label.value = mode === 'reauth' ? (account?.label || '') : (chosen?.admin_note || '');
  }
  const cookies = $('museCookies');
  if (cookies) cookies.value = '';
  const auto = $('museAutoWake');
  if (auto) auto.checked = mode === 'reauth' ? !!account?.auto_wake : true;
  const enabled = $('museEnabled');
  if (enabled) enabled.checked = mode === 'reauth' ? !!account?.enabled : true;

  const title = $('museDlgTitle');
  if (title) title.textContent = mode === 'reauth' ? '重新授权 Muse 账号' : '授权 Muse 账号';
  const desc = $('museDlgDesc');
  if (desc) desc.textContent = mode === 'reauth'
    ? `更新 ${account?.peer_id || ''} 的 Cookie；旧的授权在确认成功前不会失效。`
    : '粘贴 muse.ai 登录态 Cookie，授权后台续期与唤醒。';
  const submit = $('museDlgSubmit');
  if (submit) submit.innerHTML = `${icon('key')}${mode === 'reauth' ? '保存授权' : '授权'}`;
}

function openDialog() {
  const dlg = $('museDlg');
  if (dlg && !dlg.open) dlg.showModal();
}

export async function openMuseAuth(peerID = '', onChanged = null) {
  try {
    await ensureAccounts();
  } catch (err) {
    toast('无法读取 Muse 授权：' + err.message, 'bad');
    return;
  }
  if (peerID) {
    const existing = museState.accounts.find((a) => a.peer_id === peerID);
    if (existing) return openMuseReauth(existing, onChanged);
  }
  try {
    await ensurePeers();
  } catch (err) {
    toast('无法读取成员列表：' + err.message, 'bad');
    return;
  }
  const bound = new Set(museState.accounts.map((a) => a.peer_id));
  const candidates = museState.peers.filter((p) => p.agent_type === 'muse' && !bound.has(p.id));
  if (!candidates.length) {
    toast('所有 muse 成员都已授权；可点行内「重新授权」更新 Cookie', 'info');
    return;
  }
  const selected = peerID && candidates.some((p) => p.id === peerID) ? peerID : candidates[0].id;
  fillDialog('create', null, candidates, selected, onChanged);
  openDialog();
}

export async function openMuseReauth(account, onChanged = null) {
  if (!account) return;
  try {
    await ensurePeers();
  } catch {
    // The account already carries the peer id; a failed peer refresh must not
    // block re-authorization.
  }
  const peer = museState.peers.find((p) => p.id === account.peer_id) || {
    id: account.peer_id, display_name: account.peer_id, admin_note: '',
  };
  fillDialog('reauth', account, [peer], account.peer_id, onChanged);
  openDialog();
}

async function submitMuseForm(e) {
  e.preventDefault();
  const mode = museState.dialogMode;
  const peerID = $('musePeer')?.value || '';
  const cookies = ($('museCookies')?.value || '').trim();
  if (!peerID) return showDlgError('请选择成员');
  if (!cookies) return showDlgError('请粘贴 Muse Cookie（至少含 hatch_sess）');

  const btn = $('museDlgSubmit');
  const old = btn ? btn.innerHTML : '';
  if (btn) {
    btn.disabled = true;
    btn.innerHTML = `${icon('rotate')}${mode === 'reauth' ? '保存中…' : '授权中…'}`;
  }
  clearDlgError();
  const body = {
    label: ($('museLabel')?.value || '').trim(),
    cookies,
    auto_wake: !!$('museAutoWake')?.checked,
    enabled: !!$('museEnabled')?.checked,
  };
  try {
    let r;
    if (mode === 'reauth') {
      if (!museState.editAccount) throw new Error('缺少待更新的授权');
      r = await api(museURL(museState.editAccount.id), { method: 'PATCH', body: JSON.stringify(body) });
    } else {
      body.peer_id = peerID;
      r = await api(MUSE_ACCOUNTS, { method: 'POST', body: JSON.stringify(body) });
    }
    const warning = r.warning || (r.account?.auth_state === 'unverified' ? 'Muse 会话尚未确认，稍后保活会重试。' : '');
    if (warning) toast('已保存，但：' + warning, 'warn', 6000);
    else toast(mode === 'reauth' ? '已更新 Muse 授权' : '已授权 Muse 账号', 'ok');
    const cb = museState.onChanged;
    museState.onChanged = null;
    $('museDlg')?.close();
    await refreshMuse();
    refreshStats();
    if (cb) cb();
  } catch (err) {
    showDlgError(err.message);
    if (err.status === 409) toast('该成员已有授权；请用「重新授权」更新 Cookie', 'warn');
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.innerHTML = old;
    }
  }
}

function toastResult(res, verb) {
  if (!res) return;
  if (res.ok && res.wake_ok) {
    toast(`${verb}成功 · Noise 已验证`, 'ok');
  } else if (res.ok) {
    toast(`${verb}：会话有效，但 VM 唤醒未确认${res.error ? '：' + res.error : ''}`, 'warn', 6000);
  } else if (res.auth_state === 'needs_reauth') {
    toast(`${verb}失败：授权已失效，请重新授权`, 'bad', 6000);
  } else {
    toast(`${verb}失败：${res.error || '未知错误'}`, 'bad', 6000);
  }
}

async function keepaliveAccount(a) {
  try {
    const r = await api(museURL(a.id, 'keepalive'), { method: 'POST' });
    toastResult(r.result, '保活');
    await refreshMuse();
    refreshStats();
  } catch (err) {
    toast('保活失败：' + err.message, 'bad');
  }
}

export async function wakeMuseAccount(id) {
  try {
    const r = await api(museURL(id, 'wake'), { method: 'POST' });
    toastResult(r.result, '唤醒');
    if (museState.loaded) await refreshMuse();
    refreshStats();
    return r.result || null;
  } catch (err) {
    toast('唤醒失败：' + err.message, 'bad');
    return null;
  }
}

async function keepaliveAll() {
  const btn = $('museKeepaliveAll');
  const old = btn ? btn.innerHTML : '';
  if (btn) {
    btn.disabled = true;
    btn.innerHTML = `${icon('rotate')}保活中…`;
  }
  try {
    const r = await api(MUSE_KEEPALIVE_ALL, { method: 'POST' });
    const s = r.summary || {};
    toast(`已保活 ${s.ok || 0} 个，失败 ${s.failed || 0} 个，跳过 ${s.skipped || 0} 个`, s.failed ? 'warn' : 'ok', 5000);
    if (s.needs_reauth) toast(`${s.needs_reauth} 个账号待重新授权`, 'warn', 5000);
    await refreshMuse();
    refreshStats();
  } catch (err) {
    if (err.status === 409) toast('后台保活正在运行，请稍候再试', 'warn');
    else toast('全部保活失败：' + err.message, 'bad');
  } finally {
    if (btn) {
      btn.disabled = !!(museState.status && museState.status.running);
      btn.innerHTML = old;
    }
  }
}

async function toggleEnabled(a) {
  const want = !a.enabled;
  try {
    await api(museURL(a.id), { method: 'PATCH', body: JSON.stringify({ enabled: want }) });
    toast(`已${want ? '启用' : '停用'} ${a.peer_id} 的后台保活`, 'ok');
    await refreshMuse();
    refreshStats();
  } catch (err) {
    toast('更新失败：' + err.message, 'bad');
  }
}

async function deleteAccount(a) {
  const ok = await confirmAction({
    title: `删除 ${a.peer_id} 的 Muse 授权？`,
    body: 'Cookie 会立即从数据库删除，后台将不再续期或唤醒该账号。成员本身与中继消息不受影响。',
    danger: true,
    okText: '删除授权',
  });
  if (!ok) return;
  try {
    await api(museURL(a.id), { method: 'DELETE' });
    toast(`已删除 ${a.peer_id} 的 Muse 授权`, 'ok');
    await refreshMuse();
    refreshStats();
  } catch (err) {
    toast('删除失败：' + err.message, 'bad');
  }
}


function chatNewRequestID() {
  if (typeof crypto !== 'undefined' && crypto.randomUUID) return crypto.randomUUID();
  return 'cr-' + Date.now().toString(36) + '-' + Math.random().toString(16).slice(2);
}

export function museChatHash(peer, session = '') {
  const q = new URLSearchParams();
  if (peer) q.set('peer', peer);
  if (session) q.set('session', session);
  const qs = q.toString();
  return '#muse-chat' + (qs ? '?' + qs : '');
}

export function parseMuseChatHash(hash = location.hash) {
  const raw = String(hash || '').replace(/^#/, '');
  const i = raw.indexOf('?');
  if (i < 0) return { peer: '', session: '' };
  const q = new URLSearchParams(raw.slice(i + 1));
  return { peer: q.get('peer') || '', session: q.get('session') || '' };
}

const CHAT_CODE_TEXT = {
  muse_chat_disabled: '直接对话未启用：部署开关或运行时开关关闭。',
  deploy_flag_off: '部署开关未打开；修改配置后需重启。',
  muse_chat_access_required: '该成员尚未开通直接对话；请从成员行重新点「对话」。',
  muse_chat_in_progress: '该成员已有进行中的对话；本次请求不会排队。',
  muse_chat_busy: '后台对话并发已满，请稍后重试。',
  muse_chat_rate_limited: '发送过于频繁，请稍后重试。',
  muse_agent_busy: '成员 agent 正在处理其他消息（或 10 分钟内有未完成回复），本次未发送。',
  muse_needs_reauth: 'Muse 授权已失效，请到 Muse 保活页重新授权。',
  muse_account_unverified: 'Muse 会话尚未验证，请先在 Muse 保活页确认。',
  muse_account_disabled: '该成员的 Muse 保活已停用。',
  muse_vm_disabled: 'Muse 沙盒已禁用。',
  muse_vm_unavailable: 'Muse 沙盒不可达或未上线。',
  muse_history_unavailable: '读取会话历史失败，本次未发送。',
  muse_delivery_unknown: '投递未知：消息可能已发出。后台不会自动重发；要重发请新发一条并重新确认。',
  muse_reply_timeout: '等待回复超时；没有自动取消 Muse 侧生成。',
  muse_reply_unknown: '回复状态未知：请去 Muse 核对。',
  muse_history_reset: '会话历史出现回退，已停止等待。',
  muse_audit_unavailable: '审计写入失败，本次未发送。',
  unknown_session: '指定的 side chat 不存在；直接对话不会新建会话。',
  muse_cancelled: '已取消本地等待；已经发出的消息不会删除。',
  muse_upstream_error: 'Muse 上游返回错误。',
  muse_upstream_timeout: 'Muse 上游超时。',
  muse_upstream_rate_limited: 'Muse 上游限流，请稍后重试。',
  muse_egress_forbidden: 'Muse 网关拒绝了本次出口。',
  muse_protocol_drift: 'Muse 协议形状异常，已停止。',
};

export function museChatCodeText(code) {
  if (!code) return '';
  return CHAT_CODE_TEXT[code] || ('错误：' + code);
}

export function museChatErrorText(v) {
  if (!v || !v.error) return '';
  return museChatCodeText(v.error.code);
}

export function museChatBudget(value, max = CHAT_BUDGET_MAX) {
  const n = Number(value);
  const serverMax = Number(max) > 0 ? Math.min(CHAT_BUDGET_MAX, Number(max)) : CHAT_BUDGET_MAX;
  const upper = Math.max(CHAT_BUDGET_MIN, serverMax);
  if (!Number.isFinite(n)) return Math.min(CHAT_BUDGET_DEFAULT, upper);
  return Math.max(CHAT_BUDGET_MIN, Math.min(upper, Math.round(n)));
}

export function museChatCountdown(deadline, nowSec = Math.floor(Date.now() / 1000)) {
  if (!deadline) return '';
  const left = Math.max(0, Math.round(deadline - nowSec));
  return left + 's';
}

function accountByPeer(peer) {
  return (museState.accounts || []).find((a) => a.peer_id === peer) || null;
}

function chatThreadJobs() {
  return (museState.chatThread && museState.chatThread.jobs) || [];
}

function activeChatJob() {
  const jobs = chatThreadJobs();
  for (let i = jobs.length - 1; i >= 0; i--) {
    if (CHAT_ACTIVE.has(jobs[i].status)) return jobs[i];
  }
  return null;
}

function showChatError(msg) {
  museState.chatError = msg || '';
  const el = $('mchatErr');
  if (el) {
    el.textContent = msg || '';
    el.hidden = !msg;
  }
}

function threadJobFromView(v, text = '') {
  return {
    chat_id: v.chat_id,
    status: v.status,
    session: v.session || '',
    visibility: v.visibility || '',
    text,
    reply: v.reply || null,
    partial_text: v.partial_text || '',
    draft: !!v.draft,
    warnings: v.warnings || [],
    confidence: v.confidence || 'unknown',
    error: v.error || null,
    cancel: v.cancel || '',
    budget_secs: v.budget_secs || 0,
    user_seq: v.user_seq || 0,
    sent_at: v.sent_at || 0,
    confirmed_at: v.confirmed_at || 0,
    deadline_at: v.deadline_at || 0,
    created_at: v.created_at || Math.floor(Date.now() / 1000),
    updated_at: v.updated_at || 0,
  };
}

function mergeJobView(v) {
  if (!museState.chatThread) {
    museState.chatThread = { peer_id: museState.chatPeer, session: museState.chatSession, jobs: [], sessions: [] };
  }
  const jobs = museState.chatThread.jobs || (museState.chatThread.jobs = []);
  const idx = jobs.findIndex((j) => j.chat_id === v.chat_id);
  const text = idx >= 0 ? jobs[idx].text : '';
  const job = threadJobFromView(v, text);
  if (idx >= 0) jobs[idx] = job;
  else jobs.push(job);
  museState.chatJob = v;
  return job;
}

function renderChatReply(reply) {
  if (!reply) return '';
  const text = reply.text ? `<div class="cmd-msg-body mchat-text">${esc(reply.text)}</div>` : '';
  const media = reply.media_only ? '<p class="muted xs">（纯媒体回复，无文本）</p>' : '';
  const atts = (reply.attachments || []).map((x) => {
    const label = x.title || x.path || x.url || x.kind || '附件';
    return `<li>${esc(x.kind || '附件')} · ${esc(label)}${x.mime ? ' · ' + esc(x.mime) : ''}${x.byte_len ? ' · ' + esc(x.byte_len) + 'B' : ''}</li>`;
  }).join('');
  const list = atts ? `<ul class="mchat-attachments">${atts}</ul>` : '';
  const more = (reply.additional_replies || []).length
    ? `<p class="muted xs">另有 ${reply.additional_replies.length} 条同窗口回复（见审计 seq）</p>` : '';
  return text + media + list + more;
}

// chatStatusLine renders the clock-free part of the status line. The remaining
// seconds live in a placeholder span that refreshChatLiveText fills in after
// the markup is (or is not) written: if the seconds were part of the markup,
// every tick would change the HTML and force a full re-render.
function chatStatusLine(job) {
  if (job.reply) return '';
  let text = '';
  let tone = '';
  switch (job.status) {
    case 'preparing': text = '准备中…'; break;
    case 'sending': text = '发送中…'; break;
    case 'confirming': text = '确认投递…'; break;
    case 'waiting': text = job.deadline_at ? '已投递，等待回复 · 剩余 ' : '已投递，等待回复…'; break;
    case 'timeout': text = '已投递，等待回复超时。Muse 仍可能继续生成。'; tone = 'warn'; break;
    case 'delivery_unknown': text = museChatCodeText('muse_delivery_unknown'); tone = 'bad'; break;
    case 'reply_unknown': text = museChatCodeText('muse_reply_unknown'); tone = 'bad'; break;
    case 'cancelled': text = '已取消（cancel=' + (job.cancel || 'skipped') + '）；已发出的消息不会删除。'; tone = 'warn'; break;
    case 'failed': text = museChatErrorText(job) || '失败'; tone = 'bad'; break;
    case 'replied': text = ''; break;
    default: text = job.status || '';
  }
  if (!text && !job.error) return '';
  const parts = [];
  if (text) {
    const tick = job.status === 'waiting' && job.deadline_at
      ? `<span class="mchat-countdown" data-deadline="${Number(job.deadline_at)}"></span>`
      : '';
    parts.push(`<span class="mchat-status-line ${tone}">${esc(text)}${tick}</span>`);
  }
  if (!text && job.error) parts.push(`<span class="mchat-status-line bad">${esc(museChatErrorText(job))}</span>`);
  return parts.join('');
}

// museChatJobHTML renders one job card. It is time-independent on purpose:
// the countdown and the streamed draft are placeholders that the caller fills
// in, so the returned markup only changes when the job actually changes.
export function museChatJobHTML(job, isNew = false) {
  const when = job.created_at ? fmtTime(job.created_at) : '';
  const cls = isNew ? ' is-new' : '';
  let html = `<article class="cmd-msg msg-self${cls}">
    <div class="cmd-msg-bubble">
      <div class="cmd-msg-header"><div class="cmd-msg-author"><b>操作者</b></div><div class="cmd-msg-info"><span>${esc(when)}</span></div></div>
      <div class="cmd-msg-body mchat-text">${esc(job.text || '')}</div>
    </div>
  </article>`;
  const body = job.reply ? renderChatReply(job.reply) : chatStatusLine(job);
  const draft = !job.reply && job.partial_text
    ? `<div class="alert alert-warn" data-chat-draft="${esc(job.chat_id)}"><svg class="icon"><use href="#i-timer"/></svg><span>已有草稿（未完成，不是最终回复）：<span class="mchat-draft-text"></span></span></div>` : '';
  const peerName = esc(museState.chatPeer);
  const warnings = (job.warnings || []).length
    ? `<p class="muted xs">注意：${(job.warnings || []).map((x) => ({
      concurrent_user_activity: '期间有其他成员消息，回复归属可能不唯一',
      reply_status_not_completed: '回复状态不是 completed',
      may_need_native_approval: '回复可能涉及原生审批，请到成员 Muse 逐条处理',
      duplicate_user_text: '窗口内出现重复的用户文本，回复归属可能不唯一',
    })[x] || x).map(esc).join('；')}</p>` : '';
  if (body || draft || warnings) {
    html += `<article class="cmd-msg msg-peer${cls}">
      <div class="cmd-msg-bubble">
        <div class="cmd-msg-header"><div class="cmd-msg-author"><b>${peerName}</b></div><div class="cmd-msg-info"><span>${esc(job.updated_at ? fmtTime(job.updated_at) : '')}</span></div></div>
        ${body}${draft}${warnings}
      </div>
    </article>`;
  }
  return html;
}

// refreshChatLiveText fills the placeholders museChatJobHTML leaves behind:
// the per-second countdown and the streamed draft text. Both are plain
// textContent writes, so a tick repaints a few characters instead of the
// whole thread.
function refreshChatLiveText(stream, jobs, nowSec) {
  const byId = new Map((jobs || []).map((j) => [j.chat_id, j]));
  stream.querySelectorAll('.mchat-countdown').forEach((el) => {
    const text = museChatCountdown(Number(el.dataset.deadline || 0), nowSec);
    if (el.textContent !== text) el.textContent = text;
  });
  stream.querySelectorAll('[data-chat-draft]').forEach((el) => {
    const job = byId.get(el.dataset.chatDraft);
    const text = (job && job.partial_text) || '';
    const target = el.querySelector('.mchat-draft-text') || el;
    if (target.textContent !== text) target.textContent = text;
  });
}

function renderMuseChatHeader() {
  const peer = museState.chatPeer;
  const a = accountByPeer(peer);
  const ready = museChatReady();
  const active = activeChatJob();
  const title = $('mchatTitle');
  if (title) {
    title.textContent = peer || '选择成员';
    title.title = peer;
  }
  const rt = $('mchatRuntime');
  if (rt) {
    const cs = museState.chatStatus || {};
    const label = !cs.deploy_enabled ? '未部署' : (cs.runtime_enabled ? '对话已启用' : '运行时关闭');
    rt.textContent = label;
    rt.className = 'badge ' + (!cs.deploy_enabled ? 'muted' : (cs.runtime_enabled ? 'ok' : 'warn'));
  }
  const sess = $('mchatSession');
  if (sess && document.activeElement !== sess) {
    const seen = (museState.chatThread && museState.chatThread.sessions) || [];
    const opts = ['<option value="">主聊天</option>']
      .concat(seen.map((s) => `<option value="${esc(s)}"${s === museState.chatSession ? ' selected' : ''}>side chat · ${esc(s)}</option>`));
    if (museState.chatSession && !seen.includes(museState.chatSession)) {
      opts.push(`<option value="${esc(museState.chatSession)}" selected>side chat · ${esc(museState.chatSession)}</option>`);
    }
    const html = opts.join('');
    // Re-populating a <select> every poll tick closes an open dropdown and
    // repaints the control; the option list only changes on a session switch.
    if (sess.dataset.opts !== html) {
      sess.innerHTML = html;
      sess.dataset.opts = html;
    }
    sess.value = museState.chatSession;
  }
  const stop = $('mchatStop');
  if (stop) stop.hidden = !(active || (museState.chatJob && CHAT_ACTIVE.has(museState.chatJob.status)));
  const send = $('mchatSend');
  if (send) send.disabled = !ready || !peer || !a || !a.chat_enabled || !!active || museState.chatSending;
  const more = $('mchatDisable');
  if (more) more.disabled = !a;
}

function renderMuseChatList() {
  const el = $('mchatSessions');
  if (!el) return;
  const enabled = (museState.accounts || []).filter((a) => a.chat_enabled);
  const html = !enabled.length
    ? '<div class="cmd-empty-hint">还没有已开通直接对话的成员。<br>回到 Muse 保活页，点成员行的「对话」即可开通并进入。</div>'
    : '<div class="cmd-group-label">已开通成员</div>' + enabled.map((a) => {
      const active = a.peer_id === museState.chatPeer;
      const preview = a.last_chat_status ? museChatStatusLabel(a.last_chat_status) : '尚未对话';
      return `<div class="cmd-session-item${active ? ' active' : ''}" data-chat-peer="${esc(a.peer_id)}" role="button" tabindex="0">
      <div class="cmd-session-avatar">${avatar(a.peer_id)}</div>
      <div class="cmd-session-meta">
        <div class="cmd-session-name"><span>${esc(a.peer_id)}</span></div>
        <div class="cmd-session-preview">${esc(preview)}</div>
      </div>
    </div>`;
    }).join('');
  // Same reason as the select: rebuilding the list on every poll tick throws
  // away focus and repaints avatars for no change.
  if (el.dataset.sig === html) return;
  el.innerHTML = html;
  el.dataset.sig = html;
  el.querySelectorAll('[data-chat-peer]').forEach((row) => {
    row.onclick = () => {
      $('mchatLayout')?.classList.remove('sessions-open');
      if (row.dataset.chatPeer === museState.chatPeer) return;
      location.hash = museChatHash(row.dataset.chatPeer, '');
    };
  });
}

// museChatJobsHTML renders the whole thread. prevSeen is the set of job ids
// already on screen: only ids missing from it get the entry animation, and an
// unchanged set of jobs must yield byte-identical markup so the caller can
// skip the innerHTML write entirely (that write is what used to blink the
// thread on every tick).
export function museChatJobsHTML(jobs, prevSeen) {
  const animate = !!(prevSeen && prevSeen.size);
  return (jobs || []).map((j) => museChatJobHTML(j, animate && !prevSeen.has(j.chat_id))).join('');
}

// renderMuseChatMessages is exported for the console test lane, which drives
// it against a stub DOM to pin the "a poll tick must not rewrite the stream"
// behaviour that keeps the thread from blinking.
export function renderMuseChatMessages() {
  const stream = $('mchatStream');
  if (!stream) return;
  const peer = museState.chatPeer;
  const a = accountByPeer(peer);
  const clearState = () => { museState.chatSig = ''; museState.chatSeen = null; };
  if (!peer) {
    clearState();
    stream.innerHTML = '<div class="mchat-boundary"><svg class="icon"><use href="#i-message"/></svg><p>从 Muse 保活页的成员行点「对话」进入。<br>只显示本进程最近 1 小时的对话；更早或完整内容请去成员 Muse 查看。</p></div>';
    return;
  }
  if (!a) {
    clearState();
    stream.innerHTML = '<div class="mchat-boundary"><svg class="icon"><use href="#i-alert"/></svg><p>找不到该成员的 Muse 授权。</p></div>';
    return;
  }
  if (!museState.chatThread) {
    clearState();
    stream.innerHTML = '<div class="skel"><i></i></div>';
    return;
  }
  const jobs = chatThreadJobs();
  if (!jobs.length) {
    clearState();
    const boundary = museState.chatSession
      ? `该 side chat 在本进程最近 1 小时内没有可恢复的对话。`
      : `本进程最近 1 小时内没有可恢复的对话。`;
    stream.innerHTML = `<div class="mchat-boundary"><svg class="icon"><use href="#i-clock"/></svg><p>${boundary}<br>进程重启或超过 1 小时后，后台不再保留消息与回复；完整内容请 <a href="https://muse.ai" target="_blank" rel="noopener">去 Muse 查看</a>。</p></div>`;
    return;
  }
  const now = Math.floor(Date.now() / 1000);
  const prev = museState.chatSeen;
  const seen = new Set(jobs.map((j) => j.chat_id));
  // The signature is rendered as if every card were already on screen, so the
  // entry-animation class does not make the tick after a new message look like
  // a change (that would re-render the thread a second time for nothing).
  const sig = museChatJobsHTML(jobs, seen);
  if (museState.chatSig !== sig) {
    const nearBottom = !stream.dataset.ready ||
      stream.scrollHeight - stream.scrollTop - stream.clientHeight < 100;
    stream.innerHTML = museChatJobsHTML(jobs, prev);
    stream.dataset.ready = '1';
    museState.chatSig = sig;
    museState.chatSeen = seen;
    if (nearBottom) stream.scrollTop = stream.scrollHeight;
  }
  // Countdown seconds and the streamed draft change every tick without
  // changing the structure above; patch them in place instead of rebuilding.
  refreshChatLiveText(stream, jobs, now);
}

function renderMuseChat() {
  renderMuseChatHeader();
  renderMuseChatList();
  renderMuseChatMessages();
  showChatError(museState.chatError);
}

function chatThreadURL(peer, session) {
  let url = MUSE_CHAT + '/thread?peer_id=' + encodeURIComponent(peer || '');
  if (session) url += '&session=' + encodeURIComponent(session);
  return url;
}

function stopChatPolling() {
  museState.chatPollToken++;
  if (museState.chatTimer) {
    clearTimeout(museState.chatTimer);
    museState.chatTimer = null;
  }
}

function startMuseChatPolling(id) {
  stopChatPolling();
  if (!id) return;
  const token = museState.chatLoadToken;
  const pollToken = museState.chatPollToken;
  const peer = museState.chatPeer;
  const session = museState.chatSession;
  let failures = 0;
  const tick = async () => {
    if (!id || !museChatContextMatches({ loadToken: token, peer, session }) ||
        pollToken !== museState.chatPollToken) return;
    try {
      const v = await api(museChatURL(id));
      if (!museChatContextMatches({ loadToken: token, peer, session }) ||
          pollToken !== museState.chatPollToken) return;
      failures = 0;
      mergeJobView(v);
      if (currentRoute === 'muse-chat') renderMuseChat();
      if (CHAT_ACTIVE.has(v.status)) {
        museState.chatTimer = setTimeout(tick, 1000);
      } else {
        museState.chatTimer = null;
        await loadMuseChatThread();
        refreshMuse();
      }
    } catch (err) {
      if (!museChatContextMatches({ loadToken: token, peer, session }) ||
          pollToken !== museState.chatPollToken) return;
      if (err.status === 404) {
        museState.chatTimer = null;
        museState.chatJob = null;
        showChatError('该对话的内存快照已不可恢复（进程重启或超过 1 小时）；请去成员 Muse 查看。');
        await loadMuseChatThread();
        return;
      }
      failures++;
      if (failures >= 3) {
        museState.chatTimer = null;
        showChatError('读取对话状态失败：' + err.message);
        return;
      }
      museState.chatTimer = setTimeout(tick, 2000);
    }
  };
  museState.chatTimer = setTimeout(tick, 350);
}

async function loadMuseChatThread() {
  const token = ++museState.chatLoadToken;
  stopChatPolling();
  museState.chatLoading = true;
  museState.chatJob = null;
  if (currentRoute === 'muse-chat') renderMuseChat();
  if (!museState.chatPeer) {
    museState.chatLoading = false;
    if (currentRoute === 'muse-chat') renderMuseChat();
    return;
  }
  try {
    const thread = await api(chatThreadURL(museState.chatPeer, museState.chatSession));
    if (token !== museState.chatLoadToken) return;
    museState.chatThread = thread;
    const active = (thread.jobs || []).slice().reverse().find((j) => CHAT_ACTIVE.has(j.status)) || null;
    if (active) {
      museState.chatJob = active;
      startMuseChatPolling(active.chat_id);
    }
  } catch (err) {
    if (token !== museState.chatLoadToken) return;
    museState.chatThread = { peer_id: museState.chatPeer, session: museState.chatSession, jobs: [], sessions: [] };
    museState.chatError = '加载对话失败：' + err.message;
  } finally {
    if (token === museState.chatLoadToken) {
      museState.chatLoading = false;
      if (currentRoute === 'muse-chat') renderMuseChat();
    }
  }
}

export async function openMuseChatRoute() {
  const { peer, session } = parseMuseChatHash();
  await refreshMuse();
  let target = peer;
  if (!target) {
    const first = (museState.accounts || []).find((a) => a.chat_enabled);
    target = first ? first.peer_id : '';
    if (target) history.replaceState(null, '', museChatHash(target, session));
  }
  museState.chatThread = null;
  museState.chatJob = null;
  museState.chatPeer = target;
  museState.chatSession = session || '';
  museState.chatError = '';
  const stream = $('mchatStream');
  if (stream) delete stream.dataset.ready;
  if (!target) {
    museState.chatError = '还没有已开通直接对话的成员。回到 Muse 保活页，点成员行的「对话」即可开通并进入。';
  } else {
    const a = accountByPeer(target);
    if (!a) museState.chatError = '找不到该成员的 Muse 授权。';
    else if (!museChatReady()) museState.chatError = museChatCodeText('muse_chat_disabled');
    else if (!a.chat_enabled) museState.chatError = museChatCodeText('muse_chat_access_required');
  }
  const budget = $('mchatBudget');
  if (budget) setChatBudgetValue(budget, museChatBudget(CHAT_BUDGET_DEFAULT, museState.chatStatus?.max_budget_secs));
  if (currentRoute === 'muse-chat') renderMuseChat();
  if (target) await loadMuseChatThread();
}

async function enterMuseChat(a) {
  if (!a) return;
  if (!museChatReady()) {
    toast('直接对话未启用：需要部署开关与运行时开关同时打开', 'warn');
    return;
  }
  if (!a.chat_enabled) {
    try {
      await api(museURL(a.id, 'chat-access'), {
        method: 'POST',
        body: JSON.stringify({ enabled: true, reason: 'console' }),
      });
      a.chat_enabled = true;
      toast(`已为 ${a.peer_id} 开通直接对话；消息对该成员可见`, 'ok', 5000);
      await refreshMuse();
    } catch (err) {
      toast('开通失败：' + museChatCodeText(err.body?.code || err.message), 'bad', 6000);
      return;
    }
  }
  location.hash = museChatHash(a.peer_id);
}

async function sendChatPage(e) {
  if (e) e.preventDefault();
  const peer = museState.chatPeer;
  const session = museState.chatSession;
  const loadToken = museState.chatLoadToken;
  const a = accountByPeer(peer);
  if (!peer || !a) return;
  if (!museChatReady()) { showChatError(museChatCodeText('muse_chat_disabled')); return; }
  if (!a.chat_enabled) { showChatError(museChatCodeText('muse_chat_access_required')); return; }
  if (activeChatJob()) { showChatError('该成员已有进行中的对话；发送按钮已禁用，请等待完成或停止。'); return; }
  const text = ($('mchatText')?.value || '').trim();
  if (!text) { showChatError('请输入要发送的纯文本。'); return; }
  if ([...text].length > CHAT_TEXT_MAX) { showChatError(`文本超过 ${CHAT_TEXT_MAX} 个 Unicode 码点。`); return; }
  showChatError('');
  const budget = museChatBudget($('mchatBudget')?.value, museState.chatStatus?.max_budget_secs);
  museState.chatSending = true;
  renderMuseChatHeader();
  try {
    const v = await api(MUSE_CHAT, {
      method: 'POST',
      body: JSON.stringify({
        peer_id: peer, text, session_id: museState.chatSession || null,
        budget_secs: budget, block_secs: 0, mode: 'async',
        client_request_id: chatNewRequestID(),
      }),
    });
    if (!museChatContextMatches({ loadToken, peer, session })) {
      museState.chatSending = false;
      return;
    }
    const job = mergeJobView(v);
    job.text = text;
    const ta = $('mchatText');
    if (ta) ta.value = '';
    countChatText();
    museState.chatSending = false;
    renderMuseChat();
    startMuseChatPolling(v.chat_id);
  } catch (err) {
    museState.chatSending = false;
    if (!museChatContextMatches({ loadToken, peer, session })) {
      renderMuseChatHeader();
      return;
    }
    const body = err.body || {};
    if (err.status === 409 && body.code === 'muse_chat_in_progress' && body.chat_id) {
      toast('该成员已有进行中的对话，已附着到现有对话', 'info', 5000);
      await loadMuseChatThread();
    } else {
      showChatError('发送失败：' + museChatCodeText(body.code || err.message));
    }
    renderMuseChatHeader();
  }
}

async function cancelChatPage() {
  const job = activeChatJob() || museState.chatJob;
  if (!job) return;
  const ok = await confirmAction({
    title: '停止生成？',
    body: '只停止后台等待并尽力通知 Muse 停止生成；已经发出的 user 消息不会删除，成员仍能在 Muse 里看到。',
    danger: false,
    okText: '停止生成',
  });
  if (!ok) return;
  try {
    const r = await api(museChatURL(job.chat_id, 'cancel'), { method: 'POST', body: '{}' });
    stopChatPolling();
    mergeJobView(r.chat || { ...job, status: r.status, cancel: r.cancel });
    await loadMuseChatThread();
    toast('已停止等待', 'ok');
  } catch (err) {
    if (err.status === 409) {
      await loadMuseChatThread();
      toast('对话已结束，无需停止', 'info');
    } else {
      showChatError('取消失败：' + museChatCodeText(err.body?.code || err.message));
    }
  }
}

async function disableChatAccess() {
  const a = accountByPeer(museState.chatPeer);
  if (!a) return;
  const ok = await confirmAction({
    title: `停用 ${a.peer_id} 的直接对话？`,
    body: '停用后后台不再向该成员发送新消息，在途对话会停止等待。保活授权不受影响；再次开通只需再点一次「对话」。',
    danger: true,
    okText: '停用直接对话',
  });
  if (!ok) return;
  try {
    await api(museURL(a.id, 'chat-access'), {
      method: 'POST',
      body: JSON.stringify({ enabled: false, reason: 'console' }),
    });
    toast(`已停用 ${a.peer_id} 的直接对话`, 'ok');
    await refreshMuse();
    location.hash = '#muse';
  } catch (err) {
    showChatError('停用失败：' + museChatCodeText(err.body?.code || err.message));
  }
}

async function toggleChatRuntime() {
  const box = $('museChatRuntime');
  if (!box) return;
  const want = box.checked;
  try {
    const r = await api(MUSE_CHAT_SETTINGS, { method: 'POST', body: JSON.stringify({ enabled: want }) });
    museState.chatStatus = r.status || museState.chatStatus;
    toast(want ? '已开启直接对话运行时开关' : '已关闭直接对话；在途任务已停止等待', want ? 'ok' : 'warn', 5000);
    await refreshMuse();
  } catch (err) {
    box.checked = !want;
    toast('切换失败：' + err.message, 'bad');
  }
}

function switchChatSession(session) {
  session = (session || '').trim();
  if (session && !CHAT_SESSION_RE.test(session)) {
    showChatError('side chat session_id 只接受 1–128 个字母、数字、点、下划线、冒号或连字符。');
    return;
  }
  showChatError('');
  const hash = museChatHash(museState.chatPeer, session);
  if (location.hash === hash) {
    openMuseChatRoute();
  } else {
    location.hash = hash;
  }
}

// setChatBudgetValue keeps the select usable when the server ceiling is not one
// of the static options: muse_chat_max_budget_secs accepts any 30-300 value, and
// the panel default is that ceiling.
function setChatBudgetValue(sel, secs) {
  const value = String(secs);
  if (![...sel.options].some((o) => o.value === value)) {
    sel.add(new Option('服务端上限 ' + value + ' 秒', value));
  }
  sel.value = value;
}

function countChatText() {
  const el = $('mchatText');
  const n = el ? [...el.value].length : 0;
  const count = $('mchatCount');
  if (count) {
    count.textContent = `${n} / ${CHAT_TEXT_MAX}`;
    count.className = n > CHAT_TEXT_MAX ? 'bad xs' : 'muted xs';
  }
}

function startMuseTimer() {
  stopMuseTimer();
  const box = $('museAutoRefresh');
  if (!box || !box.checked) return;
  museState.timer = setInterval(() => {
    if (currentRoute === 'muse' && !$('app')?.hidden) refreshMuse();
  }, 15000);
}

function stopMuseTimer() {
  if (museState.timer) {
    clearInterval(museState.timer);
    museState.timer = null;
  }
}

export function stopMuse() {
  stopMuseTimer();
}

export function stopMuseChat() {
  if ($('mchatSettings')?.open) $('mchatSettings').close();
  stopChatPolling();
}

export function initMuse() {
  const settings = $('mchatSettings');
  setupDialog(settings);
  $('mchatSettingsBtn').onclick = () => {
    $('mchatSessionInput').value = museState.chatSession;
    settings.showModal();
  };
  $('mchatRuntimeLink').onclick = () => settings.close();
  const dlg = $('museDlg');
  if (dlg) {
    setupDialog(dlg);
    dlg.addEventListener('close', () => {
      museState.onChanged = null;
      museState.editAccount = null;
    });
  }
  const form = $('museForm');
  if (form) form.onsubmit = submitMuseForm;

  const chatForm = $('mchatForm');
  if (chatForm) chatForm.onsubmit = sendChatPage;
  const chatText = $('mchatText');
  if (chatText) chatText.oninput = countChatText;
  const chatStop = $('mchatStop');
  if (chatStop) chatStop.onclick = cancelChatPage;
  const chatDisable = $('mchatDisable');
  if (chatDisable) chatDisable.onclick = () => { settings.close(); disableChatAccess(); };
  const chatSession = $('mchatSession');
  if (chatSession) chatSession.onchange = () => switchChatSession(chatSession.value);
  const chatSessionApply = $('mchatSessionApply');
  if (chatSessionApply) chatSessionApply.onclick = () => {
    settings.close();
    switchChatSession($('mchatSessionInput').value);
  };
  const chatBack = $('mchatBackMuse');
  if (chatBack) chatBack.onclick = () => { location.hash = '#muse'; };
  const chatListBtn = $('mchatListBtn');
  if (chatListBtn) chatListBtn.onclick = () => $('mchatLayout')?.classList.toggle('sessions-open');
  const chatRuntime = $('museChatRuntime');
  if (chatRuntime) chatRuntime.onchange = toggleChatRuntime;
  window.addEventListener('hashchange', () => {
    if (currentRoute !== 'muse-chat') return;
    const { peer, session } = parseMuseChatHash();
    if (peer !== museState.chatPeer || session !== museState.chatSession) openMuseChatRoute();
  });

  const refresh = $('museRefresh');
  if (refresh) {
    refresh.onclick = async () => {
      const ok = await refreshMuse();
      if (ok) toast('已刷新 Muse 授权', 'ok', 1500);
    };
  }
  const auth = $('museAuthorize');
  if (auth) auth.onclick = () => openMuseAuth('', refreshMuse);
  const all = $('museKeepaliveAll');
  if (all) all.onclick = keepaliveAll;

  const auto = $('museAutoRefresh');
  if (auto) {
    auto.onchange = () => {
      startMuseTimer();
      if (auto.checked) toast('已开启自动刷新 (15s)', 'info', 2000);
    };
  }

  const seg = $('museSeg');
  if (seg) {
    seg.querySelectorAll('button').forEach((b) => {
      b.onclick = () => {
        seg.querySelectorAll('button').forEach((x) => x.classList.remove('on'));
        b.classList.add('on');
        museState.filter = b.dataset.v || '';
        renderMuse();
      };
    });
  }

  const q = $('museQ');
  let timer = null;
  if (q) {
    q.oninput = () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        museState.q = q.value.trim();
        renderMuse();
      }, 220);
    };
  }
}
