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
};

const MUSE_ACCOUNTS = '/admin/muse/accounts';
const MUSE_KEEPALIVE_ALL = '/admin/muse/keepalive';

// Parameterised admin URL. Kept as concatenation so the console's route
// contract test sees a real prefix (/admin/muse/accounts) rather than a
// template literal it would probe with a bogus id.
export const museURL = (id, action = '') =>
  MUSE_ACCOUNTS + '/' + encodeURIComponent(id) + (action ? '/' + action : '');

export function museAuthTone(a) {
  if (!a) return 'muted';
  if (!a.enabled) return 'muted';
  if (a.auth_state === 'needs_reauth') return 'bad';
  if (a.auth_state === 'ok') return a.last_attempt_ok && !a.last_error ? 'ok' : 'warn';
  return 'warn';
}

export function museAuthLabel(a) {
  if (!a) return '—';
  if (!a.enabled) return '已停用';
  if (a.auth_state === 'needs_reauth') return '待重新授权';
  if (a.auth_state === 'unverified') return '未验证';
  if (a.auth_state === 'ok' && a.last_attempt_ok && !a.last_error) return '正常';
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
  if (vm === 'DISABLED') return 'muted';
  if (!vm) return 'muted';
  return 'warn';
}

const toneClass = (tone) => (tone && tone !== 'muted' ? ' ' + tone : '');

const authBadge = (a) => {
  const tip = [a.auth_state, a.vm_state, a.last_error].filter(Boolean).join(' · ');
  return `<span class="badge${toneClass(museAuthTone(a))}" title="${esc(tip)}">${esc(museAuthLabel(a))}</span>`;
};

const vmBadge = (a) => {
  const vm = a.vm_state || '—';
  return `<span class="badge${toneClass(museVMTone(a.vm_state))}">${esc(vm)}</span>`;
};

const wakeCell = (a) => {
  if (!a.last_wake_at) return '<span class="muted xs">—</span>';
  let tone = 'warn';
  let label = '未上线';
  if (a.wake_recovered) { tone = 'ok'; label = '已上线'; }
  else if (a.wake_pending) { tone = 'info'; label = '唤醒中'; }
  return `<div class="ver-stack"><span class="badge ${tone}">${label}</span>` +
    `<small class="muted xs" title="${esc(fmtTime(a.last_wake_at))}">${esc(ago(a.last_wake_at))}</small></div>`;
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
  return `<div class="row-btns tight">
    <button class="btn-icon" data-act="keepalive" data-id="${esc(a.id)}" data-tip="立即保活" aria-label="立即保活">${icon('refresh')}</button>
    <button class="btn-icon" data-act="wake" data-id="${esc(a.id)}" data-tip="唤醒 Muse 沙盒" aria-label="唤醒 Muse 沙盒">${icon('zap')}</button>
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
    if (museState.filter === 'ok' && !(a.auth_state === 'ok' && a.last_attempt_ok)) return false;
    if (museState.filter === 'needs_reauth' && a.auth_state !== 'needs_reauth') return false;
    if (museState.filter === 'offline' && a.peer_online) return false;
    if (q) {
      const hay = `${a.peer_id} ${a.label || ''} ${a.last_error || ''}`.toLowerCase();
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
  set('museFcReauth', f.needs_reauth ?? 0);
  set('museFcOffline', f.offline ?? 0);
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
  if (!cookies) return showDlgError('请粘贴 Muse Cookie（至少 hatch_sess + hatch_vml）');

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
    toast(`${verb}成功 · VM RUNNING`, 'ok');
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
