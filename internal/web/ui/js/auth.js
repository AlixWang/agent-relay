// Authentication & stats management for agent-relay console
import { api } from './api.js';
import { $, esc, icon, toast } from './utils.js';
import { setRoute } from './router.js';

export let serverPromptVersion = 0;
export let serverClientVersion = '';
export let serverReceiverRev = '';

export function showLogin(msg) {
  $('app').hidden = true;
  $('loginPage').hidden = false;
  const errEl = $('loginErr');
  if (msg) {
    errEl.innerHTML = `${icon('alert')}<span>${esc(msg)}</span>`;
    errEl.hidden = false;
  } else {
    errEl.hidden = true;
  }
}

export function showApp() {
  $('loginPage').hidden = true;
  $('app').hidden = false;
}

export async function refreshStats() {
  try {
    const s = await api('/admin/stats');
    if (s.protocol) {
      const verEl = $('sbVer');
      if (verEl) verEl.textContent = `v${s.protocol} · ${s.client_version || 'dev'}`;
    }
    const sbSeq = $('sbSeq');
    if (sbSeq) sbSeq.textContent = s.max_seq ?? 0;
    const sbDb = $('sbDb');
    if (sbDb) sbDb.textContent = `${((s.db_bytes || 0) / 1024).toFixed(0)} KiB`;

    const onlineStr = `${s.peers_online || 0} / ${s.peers_total || 0}`;
    const onlineText = $('onlineText');
    if (onlineText) onlineText.textContent = `${onlineStr} 在线`;
    const stOnline = $('stOnline');
    if (stOnline) stOnline.innerHTML = `${s.peers_online || 0} <small>/ ${s.peers_total || 0}</small>`;
    const navOnline = $('navOnline');
    if (navOnline) navOnline.textContent = `${s.peers_online || 0}`;

    const pending = s.pending_approvals || 0;
    const stPending = $('stPending');
    if (stPending) stPending.textContent = pending;
    const navPending = $('navPending');
    if (navPending) {
      if (pending > 0) {
        navPending.textContent = pending;
        navPending.hidden = false;
      } else {
        navPending.hidden = true;
      }
    }

    const stThreads = $('stThreads');
    if (stThreads) stThreads.textContent = s.threads_total || 0;
    const stTokens = $('stTokens');
    if (stTokens) stTokens.textContent = s.tokens_active || 0;

    const navInbox = $('navInbox');
    if (navInbox) {
      const inbox = s.inbox_unread || 0;
      navInbox.textContent = inbox > 0 ? inbox : '';
      navInbox.hidden = inbox === 0;
    }

    if (s.prompt_version) serverPromptVersion = s.prompt_version;
    if (s.client_version) serverClientVersion = s.client_version;
    if (s.receiver_rev) serverReceiverRev = s.receiver_rev;

    showApp();
    return true;
  } catch (e) {
    if (e.status === 401) showLogin();
    return false;
  }
}

export function initAuth() {
  const form = $('loginForm');
  if (form) {
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const btn = $('loginbtn');
      btn.disabled = true;
      btn.classList.add('loading');
      try {
        await api('/admin/login', {
          method: 'POST',
          body: JSON.stringify({ username: $('username').value, password: $('pw').value }),
        });
        $('pw').value = '';
        showApp();
        toast('登录成功', 'ok');
        await refreshStats();
        setRoute(location.hash.replace(/^#/, '') || 'members');
      } catch (err) {
        showLogin('登录失败：' + err.message);
      } finally {
        btn.disabled = false;
        btn.classList.remove('loading');
      }
    });
  }

  const logoutBtn = $('logoutbtn');
  if (logoutBtn) {
    logoutBtn.onclick = async () => {
      await api('/admin/logout', { method: 'POST' }).catch(() => {});
      showLogin();
      toast('已安全退出', 'info');
    };
  }
}
