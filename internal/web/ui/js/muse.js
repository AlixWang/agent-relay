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
  chatAccount: null,
  chatJob: null,
  chatTimer: null,
  chatConsentAccount: null,
};

const MUSE_ACCOUNTS = '/admin/muse/accounts';
const MUSE_KEEPALIVE_ALL = '/admin/muse/keepalive';
const MUSE_CHAT = '/admin/muse/chat';
const MUSE_CHAT_SETTINGS = '/admin/muse/chat/settings';

export const museChatURL = (id, action = '') =>
  MUSE_CHAT + '/' + encodeURIComponent(id) + (action ? '/' + action : '');

const CHAT_CONSENT_TEXT = '我同意 agent-relay 后台可以以我的 muse.ai 登录身份，向我指定的 Muse agent 发送操作者输入的消息；'
  + '该消息会出现在我的 Muse 主聊天或我指定的 side chat，我本人可见。agent 可能因此执行操作或调用工具；'
  + '此通道不是审批通道，不能代替我在 Muse 中点击原生审批卡。我可以随时要求关闭；关闭后后台不再发送新消息。';

const CHAT_ACTIVE = new Set(['preparing', 'sending', 'confirming', 'waiting']);

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
    : (a.chat_enabled ? '与成员 Muse agent 对话' : '开通直接对话（需成员同意）');
  const chatBtn = `<button class="btn-icon${a.chat_enabled ? '' : ' warn'}" data-act="${a.chat_enabled ? 'chat' : 'consent'}" data-id="${esc(a.id)}" data-tip="${esc(chatTip)}" aria-label="${esc(chatTip)}" ${chatReady ? '' : 'disabled'}>${icon('message')}</button>`;
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
          await openMuseChat(a);
          break;
        case 'consent':
          openChatConsent(a);
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


function showChatDialogError(id, msg) {
  const el = $(id);
  if (!el) return;
  el.textContent = msg || '';
  el.hidden = !msg;
}

function chatNewRequestID() {
  if (typeof crypto !== 'undefined' && crypto.randomUUID) return crypto.randomUUID();
  return 'cr-' + Date.now().toString(36) + '-' + Math.random().toString(16).slice(2);
}

function openChatConsent(a) {
  if (!a || !museChatReady()) {
    toast('直接对话未启用：需要部署开关与运行时开关同时打开', 'warn');
    return;
  }
  museState.chatConsentAccount = a;
  const peer = $('museChatConsentPeer');
  if (peer) peer.textContent = a.peer_id;
  const text = $('museChatConsentText');
  if (text) text.textContent = CHAT_CONSENT_TEXT;
  const evidence = $('museConsentEvidence');
  if (evidence) evidence.value = '';
  const check = $('museConsentCheck');
  if (check) check.checked = false;
  showChatDialogError('museConsentErr', '');
  const dlg = $('museChatConsentDlg');
  if (dlg && !dlg.open) dlg.showModal();
}

async function submitChatConsent(e) {
  e.preventDefault();
  const a = museState.chatConsentAccount;
  if (!a) return;
  const evidence = ($('museConsentEvidence')?.value || '').trim();
  if ([...evidence].length < 8) return showChatDialogError('museConsentErr', '请填写至少 8 个字符的同意证据（如成员在中继里说“我同意”的消息 id）');
  if (!$('museConsentCheck')?.checked) return showChatDialogError('museConsentErr', '请先勾选“我确认已取得该成员同意”');
  const btn = $('museConsentSubmit');
  const old = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = `${icon('rotate')}开通中…`; }
  try {
    await api('/admin/muse/accounts/' + encodeURIComponent(a.id) + '/chat-consent', {
      method: 'POST',
      body: JSON.stringify({ enabled: true, consent_version: 1, consent_evidence: evidence }),
    });
    toast(`已为 ${a.peer_id} 开通直接对话；消息对该成员可见`, 'ok', 5000);
    $('museChatConsentDlg')?.close();
    await refreshMuse();
    refreshStats();
  } catch (err) {
    showChatDialogError('museConsentErr', err.message);
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = old; }
  }
}

export async function openMuseChat(a) {
  if (!a) return;
  if (!museChatReady()) {
    toast('直接对话未启用：需要部署开关与运行时开关同时打开', 'warn');
    return;
  }
  museState.chatAccount = a;
  museState.chatJob = null;
  const title = $('museChatTitle');
  if (title) title.textContent = `与 ${a.peer_id} 的 Muse agent 对话`;
  const vis = $('museChatVisibility');
  if (vis) {
    vis.textContent = `这条消息会进入 ${a.peer_id} 的 Muse 主聊天（或下方指定的 side chat），成员本人可见；`
      + 'agent 可能因此执行操作或调用工具。此通道不是审批通道，不能代替成员在 Muse 中点击原生审批卡。';
  }
  const text = $('museChatText');
  if (text) text.value = '';
  const sess = $('museChatSession');
  if (sess) sess.value = '';
  const confirmPeer = $('museChatConfirmPeer');
  if (confirmPeer) confirmPeer.value = '';
  const confirmField = $('museChatConfirmField');
  if (confirmField) {
    let seen = false;
    try { seen = !!sessionStorage.getItem('museChatConfirmed:' + a.peer_id); } catch { seen = false; }
    confirmField.hidden = seen;
  }
  const count = $('museChatCount');
  if (count) count.textContent = '0 / 4000';
  const budget = $('museChatBudget');
  if (budget) budget.value = '180';
  renderChatJob(null);
  showChatDialogError('museChatErr', '');
  const dlg = $('museChatDlg');
  if (dlg && !dlg.open) dlg.showModal();
}

function renderChatReply(reply) {
  if (!reply) return '';
  const text = reply.text ? `<div class="chat-reply-text">${esc(reply.text)}</div>` : '';
  const media = reply.media_only ? '<p class="muted xs">（纯媒体回复，无文本）</p>' : '';
  const atts = (reply.attachments || []).map((x) => {
    const label = x.title || x.path || x.url || x.kind || '附件';
    return `<li>${esc(x.kind || '附件')} · ${esc(label)}${x.mime ? ' · ' + esc(x.mime) : ''}${x.byte_len ? ' · ' + esc(x.byte_len) + 'B' : ''}</li>`;
  }).join('');
  const list = atts ? `<ul class="chat-atts">${atts}</ul>` : '';
  const more = (reply.additional_replies || []).length
    ? `<p class="muted xs">另有 ${reply.additional_replies.length} 条同窗口回复（见审计 seq）</p>` : '';
  return text + media + list + more;
}

function renderChatJob(v) {
  museState.chatJob = v;
  const badge = $('museChatState');
  if (badge) {
    badge.textContent = v ? museChatStatusLabel(v.status) : '未发送';
    badge.className = 'badge' + (v ? (CHAT_ACTIVE.has(v.status) ? ' info' : (v.status === 'replied' ? ' ok' : (v.status === 'failed' ? ' bad' : ' warn'))) : ' muted');
  }
  const meta = $('museChatMeta');
  if (meta) {
    meta.textContent = v
      ? `${v.chat_id} · baseline ${v.baseline_seq ?? '—'} · user_seq ${v.user_seq || '—'} · poll ${v.poll_count || 0} · ${v.visibility || ''}`
      : '发送后这里显示 job 时间线。';
  }
  const reply = $('museChatReply');
  if (reply) {
    reply.innerHTML = v && v.reply ? renderChatReply(v.reply) : '';
    if (v && !v.reply && v.partial_text) {
      reply.innerHTML = `<div class="alert alert-warn"><svg class="icon"><use href="#i-timer"/></svg><span>已停止等待；已有草稿（未完成，不是最终回复）：<b>${esc(v.partial_text)}</b></span></div>`;
    }
  }
  const warn = $('museChatWarnings');
  if (warn) {
    const items = (v && v.warnings) || [];
    warn.hidden = !items.length;
    warn.textContent = items.length ? '注意：' + items.map((x) => ({
      concurrent_user_activity: '期间有其他成员消息，回复归属可能不唯一',
      reply_status_not_completed: '回复状态不是 completed',
      may_need_native_approval: '回复可能涉及原生审批，请到成员 Muse 逐条处理',
    })[x] || x).join('；') : '';
  }
  const err = $('museChatErr');
  if (err) {
    err.hidden = !(v && v.error);
    err.textContent = v && v.error ? `错误：${v.error.code}${v.error.retryable ? '（可重试）' : ''}` : '';
  }
  const cancel = $('museChatCancel');
  if (cancel) cancel.disabled = !(v && CHAT_ACTIVE.has(v.status));
  const send = $('museChatSend');
  if (send && !send.disabled) send.innerHTML = `${icon('send')}发送`;
}

function stopChatPolling() {
  if (museState.chatTimer) {
    clearTimeout(museState.chatTimer);
    museState.chatTimer = null;
  }
}

function startChatPolling(id) {
  stopChatPolling();
  const tick = async () => {
    if (!id || !museState.chatAccount) return;
    try {
      const v = await api(museChatURL(id));
      renderChatJob(v);
      if (CHAT_ACTIVE.has(v.status)) {
        museState.chatTimer = setTimeout(tick, 1000);
      } else {
        museState.chatTimer = null;
        await refreshMuse();
      }
    } catch (err) {
      showChatDialogError('museChatErr', '读取对话状态失败：' + err.message);
      museState.chatTimer = null;
    }
  };
  museState.chatTimer = setTimeout(tick, 350);
}

async function sendChat(e) {
  e.preventDefault();
  const a = museState.chatAccount;
  if (!a) return;
  const text = ($('museChatText')?.value || '').trim();
  if (!text) return showChatDialogError('museChatErr', '请输入要发送的纯文本');
  if ([...text].length > 4000) return showChatDialogError('museChatErr', '文本超过 4000 个 Unicode 码点');
  const confirmKey = 'museChatConfirmed:' + a.peer_id;
  let seenBefore = false;
  try { seenBefore = !!sessionStorage.getItem(confirmKey); } catch { seenBefore = false; }
  if (!seenBefore && ($('museChatConfirmPeer')?.value || '').trim() !== a.peer_id) {
    return showChatDialogError('museChatErr', `首次发送请精确输入成员 peer_id：${a.peer_id}`);
  }
  const sessionID = ($('museChatSession')?.value || '').trim();
  const budget = Number($('museChatBudget')?.value || 180);
  const ok = await confirmAction({
    title: `发送给 ${a.peer_id}？`,
    body: `这条消息会进入 ${a.peer_id} 的 Muse 主聊天${sessionID ? '（指定 side chat）' : ''}，成员本人可见；agent 可能执行操作。此通道不是审批通道。`,
    okText: '确认发送',
  });
  if (!ok) return;
  const btn = $('museChatSend');
  const old = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = `${icon('rotate')}发送中…`; }
  showChatDialogError('museChatErr', '');
  try {
    const v = await api(MUSE_CHAT, {
      method: 'POST',
      body: JSON.stringify({
        peer_id: a.peer_id, text, session_id: sessionID || null,
        budget_secs: budget, block_secs: 0, mode: 'async',
        client_request_id: chatNewRequestID(),
      }),
    });
    try { sessionStorage.setItem(confirmKey, '1'); } catch { /* storage may be disabled */ }
    const confirmField = $('museChatConfirmField');
    if (confirmField) confirmField.hidden = true;
    renderChatJob(v);
    startChatPolling(v.chat_id);
  } catch (err) {
    showChatDialogError('museChatErr', '发送失败：' + err.message);
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = old; }
  }
}

async function cancelChat() {
  const v = museState.chatJob;
  if (!v) return;
  const ok = await confirmAction({
    title: '停止生成？',
    body: '只停止后台等待并尽力通知 Muse 停止生成；已经发出的 user 消息不会删除，成员仍能在 Muse 里看到。',
    danger: false,
    okText: '停止生成',
  });
  if (!ok) return;
  try {
    const r = await api(museChatURL(v.chat_id, 'cancel'), { method: 'POST', body: '{}' });
    renderChatJob(r.chat || { ...v, status: r.status, cancel: r.cancel });
    stopChatPolling();
    await refreshMuse();
  } catch (err) {
    showChatDialogError('museChatErr', '取消失败：' + err.message);
  }
}

async function disableChat() {
  const a = museState.chatAccount;
  if (!a) return;
  const ok = await confirmAction({
    title: `关闭 ${a.peer_id} 的直接对话？`,
    body: '关闭后后台不再发送新消息，在途任务会停止等待。保活授权不受影响；再次开启需要重新确认成员同意。',
    danger: true,
    okText: '关闭直接对话',
  });
  if (!ok) return;
  try {
    await api('/admin/muse/accounts/' + encodeURIComponent(a.id) + '/chat-consent', {
      method: 'POST', body: JSON.stringify({ enabled: false, reason: 'console' }),
    });
    toast(`已关闭 ${a.peer_id} 的直接对话`, 'ok');
    $('museChatDlg')?.close();
    await refreshMuse();
  } catch (err) {
    showChatDialogError('museChatErr', '关闭失败：' + err.message);
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

function countChatText() {
  const el = $('museChatText');
  const n = el ? [...el.value].length : 0;
  const count = $('museChatCount');
  if (count) {
    count.textContent = `${n} / 4000`;
    count.className = n > 4000 ? 'bad xs' : 'muted xs';
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

export function initMuse() {
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

  const consentDlg = $('museChatConsentDlg');
  if (consentDlg) {
    setupDialog(consentDlg);
    consentDlg.addEventListener('close', () => { museState.chatConsentAccount = null; });
  }
  const consentForm = $('museChatConsentForm');
  if (consentForm) consentForm.onsubmit = submitChatConsent;

  const chatDlg = $('museChatDlg');
  if (chatDlg) {
    setupDialog(chatDlg);
    chatDlg.addEventListener('close', () => {
      stopChatPolling();
      museState.chatAccount = null;
      museState.chatJob = null;
    });
  }
  const chatForm = $('museChatForm');
  if (chatForm) chatForm.onsubmit = sendChat;
  const chatText = $('museChatText');
  if (chatText) chatText.oninput = countChatText;
  const chatCancel = $('museChatCancel');
  if (chatCancel) chatCancel.onclick = cancelChat;
  const chatDisable = $('museChatDisable');
  if (chatDisable) chatDisable.onclick = disableChat;
  const chatRuntime = $('museChatRuntime');
  if (chatRuntime) chatRuntime.onchange = toggleChatRuntime;

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
