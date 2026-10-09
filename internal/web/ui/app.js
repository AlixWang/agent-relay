// agent-relay operations console client (DESIGN §4.8, §9)

/* ---------- Helpers & Utilities ---------- */
async function api(path, opts = {}) {
  const o = { headers: { 'Content-Type': 'application/json' }, ...opts };
  o.headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  const r = await fetch(path, o);
  let body = null;
  try { body = await r.json(); } catch { body = null; }
  if (!r.ok) {
    const msg = (body && body.error) || r.statusText || ('HTTP ' + r.status);
    const err = new Error(msg);
    err.status = r.status;
    err.body = body;
    throw err;
  }
  return body;
}

const msgPayloadMap = new Map();

async function copyToClipboard(text) {
  if (text == null) return false;
  const str = String(text);
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(str);
      return true;
    } catch (_) {}
  }
  try {
    const ta = document.createElement('textarea');
    ta.value = str;
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    ta.style.top = '-9999px';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    if (ok) return true;
  } catch (_) {}
  return false;
}

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
}[c]));

const icon = (name, cls = '') =>
  `<svg class="icon ${cls}"><use href="#i-${name}"/></svg>`;

const ago = (ts) => {
  if (!ts) return '从未';
  const d = Math.floor(Date.now() / 1000 - ts);
  if (d < 5) return '刚刚';
  if (d < 60) return d + 's 前';
  if (d < 3600) return Math.floor(d / 60) + 'm 前';
  if (d < 86400) return Math.floor(d / 3600) + 'h 前';
  return Math.floor(d / 86400) + 'd 前';
};

const fmtTime = (ts) => {
  if (!ts) return '-';
  const d = new Date(ts * 1000);
  const pad = (n) => String(n).padStart(2, '0');
  const Y = d.getFullYear();
  const M = pad(d.getMonth() + 1);
  const D = pad(d.getDate());
  const h = pad(d.getHours());
  const m = pad(d.getMinutes());
  const s = pad(d.getSeconds());
  return `${Y}/${M}/${D} ${h}:${m}:${s}`;
};

const hashHue = (str) => {
  let h = 0;
  for (let i = 0; i < str.length; i++) {
    h = ((h << 5) - h + str.charCodeAt(i)) & 0xffffffff;
  }
  return Math.abs(h) % 360;
};

const avatar = (id, cls = '') => {
  const safe = String(id || '?');
  const hue = hashHue(safe);
  const initials = safe.slice(0, 2).toUpperCase();
  return `<div class="avatar ${cls}" style="--h: ${hue}">${esc(initials)}</div>`;
};

/* ---------- Toast Notifications ---------- */
function toast(message, type = 'info', duration = 3200) {
  const container = $('toasts');
  if (!container) return;
  const t = document.createElement('div');
  t.className = `toast toast-${type} ${type}`;
  const ic = type === 'ok' ? 'ok' : (type === 'bad' ? 'err' : (type === 'warn' ? 'alert' : 'info'));
  t.innerHTML = `${icon(ic)}<p>${esc(message)}</p><button aria-label="关闭">${icon('x', 'xs')}</button>`;
  const close = () => {
    t.classList.add('leaving');
    setTimeout(() => t.remove(), 200);
  };
  t.querySelector('button').onclick = close;
  container.appendChild(t);
  if (duration > 0) {
    setTimeout(close, duration);
  }
}

/* ---------- Modal Dialog Helpers (with light-dismiss fallback) ---------- */
function setupDialog(dlg) {
  if (!dlg) return;
  // Fallback for browsers without native closedby support
  if (!('closedBy' in HTMLDialogElement.prototype)) {
    dlg.addEventListener('click', (e) => {
      if (e.target !== dlg) return;
      const rect = dlg.getBoundingClientRect();
      const isContent = (
        rect.top <= e.clientY && e.clientY <= rect.top + rect.height &&
        rect.left <= e.clientX && e.clientX <= rect.left + rect.width
      );
      if (!isContent) dlg.close();
    });
  }
  dlg.querySelectorAll('[data-close]').forEach((btn) => {
    btn.onclick = () => dlg.close();
  });
}

function confirmAction({ title = '确认操作', body = '此操作不可撤销，是否继续？', danger = true, okText = '确认' }) {
  const dlg = $('confirmDlg');
  if (!dlg) return Promise.resolve(confirm(body));
  $('cfTitle').textContent = title;
  $('cfBody').textContent = body;
  const okBtn = $('cfOk');
  okBtn.textContent = okText;
  okBtn.className = danger ? 'btn btn-danger' : 'btn btn-primary';
  const ic = $('cfIc');
  ic.className = danger ? 'card-ic bad' : 'card-ic warn';
  ic.innerHTML = icon(danger ? 'trash' : 'alert');

  return new Promise((resolve) => {
    const handler = () => {
      dlg.removeEventListener('close', handler);
      resolve(dlg.returnValue === 'ok');
    };
    dlg.addEventListener('close', handler);
    dlg.showModal();
  });
}

/* ---------- Theme switcher ---------- */
function initTheme() {
  const btn = $('themeBtn');
  const getTheme = () => document.documentElement.dataset.theme || 'dark';
  const applyTheme = (t) => {
    document.documentElement.dataset.theme = t;
    try { localStorage.setItem('ar-theme', t); } catch {}
    if (btn) {
      btn.innerHTML = icon(t === 'dark' ? 'sun' : 'moon');
      btn.setAttribute('data-tip', t === 'dark' ? '切换浅色主题' : '切换深色主题');
    }
  };
  applyTheme(getTheme());
  if (btn) {
    btn.onclick = () => applyTheme(getTheme() === 'dark' ? 'light' : 'dark');
  }
}

/* ---------- Pagination Component ---------- */
function renderPager(targetEl, { total, page, pageSize, onPage, onPageSize }) {
  if (!targetEl) return;
  if (!total || total <= 0) {
    targetEl.innerHTML = '';
    return;
  }
  const totalPages = Math.ceil(total / pageSize) || 1;
  const start = (page - 1) * pageSize + 1;
  const end = Math.min(page * pageSize, total);

  // Generate page numbers
  const pages = [];
  if (totalPages <= 7) {
    for (let i = 1; i <= totalPages; i++) pages.push(i);
  } else {
    pages.push(1);
    if (page > 3) pages.push('...');
    const pStart = Math.max(2, page - 1);
    const pEnd = Math.min(totalPages - 1, page + 1);
    for (let i = pStart; i <= pEnd; i++) pages.push(i);
    if (page < totalPages - 2) pages.push('...');
    pages.push(totalPages);
  }

  targetEl.innerHTML = `
    <div class="pager-info">
      <span>第 <b>${start}</b> - <b>${end}</b> 条，共 <b>${total}</b> 条</span>
      <select class="pager-size" aria-label="每页条数">
        ${[10, 20, 50, 100].map((s) => `<option value="${s}" ${s === pageSize ? 'selected' : ''}>${s} 条/页</option>`).join('')}
      </select>
    </div>
    <div class="pager-nav">
      <button class="p-prev-all" ${page <= 1 ? 'disabled' : ''} title="第一页">${icon('chevs-l')}</button>
      <button class="p-prev" ${page <= 1 ? 'disabled' : ''} title="上一页">${icon('chev-l')}</button>
      ${pages.map((p) => {
        if (p === '...') return `<span class="gap">…</span>`;
        return `<button class="p-num ${p === page ? 'on' : ''}" data-p="${p}">${p}</button>`;
      }).join('')}
      <button class="p-next" ${page >= totalPages ? 'disabled' : ''} title="下一页">${icon('chev-r')}</button>
      <button class="p-next-all" ${page >= totalPages ? 'disabled' : ''} title="最后页">${icon('chevs-r')}</button>
    </div>
  `;

  targetEl.querySelector('.pager-size').onchange = (e) => {
    onPageSize?.(Number(e.target.value));
  };
  targetEl.querySelector('.p-prev-all').onclick = () => onPage?.(1);
  targetEl.querySelector('.p-prev').onclick = () => onPage?.(Math.max(1, page - 1));
  targetEl.querySelector('.p-next').onclick = () => onPage?.(Math.min(totalPages, page + 1));
  targetEl.querySelector('.p-next-all').onclick = () => onPage?.(totalPages);
  targetEl.querySelectorAll('.p-num').forEach((b) => {
    b.onclick = () => onPage?.(Number(b.dataset.p));
  });
}

/* ---------- Global State ---------- */
let serverPromptVersion = 0;
let serverClientVersion = '';
let serverReceiverRev = '';
let currentRoute = 'members';
let peerRefreshTimer = null;

const routeMeta = {
  members: { title: '成员', desc: '注册助手状态、在线心跳与路由能力' },
  threads: { title: '消息 & 线程', desc: '会话流转、熔断保护与安全审批' },
  prompts: { title: '邀请 & Prompt', desc: '助手入驻凭据签发与版本指令生成' },
  audit:   { title: '审计日志', desc: '管理事件、状态流转与安全审计追踪' },
  tokens:  { title: 'Token 管理', desc: '凭证轮换、前缀核验与实时吊销' },
  system:  { title: '策略 & 更新', desc: '运行策略参数、服务规格与自动升级' },
};

/* ---------- Route Manager ---------- */
function setRoute(r) {
  if (!routeMeta[r]) r = 'members';
  currentRoute = r;
  location.hash = r;

  document.querySelectorAll('.sb-link').forEach((l) => {
    l.classList.toggle('active', l.dataset.route === r);
  });
  document.querySelectorAll('main > .page').forEach((sec) => {
    sec.hidden = sec.id !== `page-${r}`;
  });

  const m = routeMeta[r];
  $('pageTitle').textContent = m.title;
  $('pageDesc').textContent = m.desc;

  // Close mobile sidebar if open
  $('app').classList.remove('nav-open');

  // Trigger page-specific refresh
  if (r === 'members') refreshPeers();
  else if (r === 'threads') refreshThreads();
  else if (r === 'audit') refreshAudit();
  else if (r === 'tokens') refreshTokens();
  else if (r === 'system') refreshSystem();
}

function initNavigation() {
  document.querySelectorAll('.sb-link').forEach((l) => {
    l.onclick = (e) => {
      e.preventDefault();
      setRoute(l.dataset.route);
    };
  });
  window.addEventListener('hashchange', () => {
    const h = location.hash.replace(/^#/, '');
    if (h && h !== currentRoute) setRoute(h);
  });

  const menuBtn = $('menuBtn');
  const scrim = $('sbScrim');
  if (menuBtn) {
    menuBtn.onclick = () => $('app').classList.toggle('nav-open');
  }
  if (scrim) {
    scrim.onclick = () => $('app').classList.remove('nav-open');
  }
}

/* ---------- Login & Auth ---------- */
function showLogin(msg) {
  $('app').hidden = true;
  $('loginPage').hidden = false;
  if (peerRefreshTimer) clearInterval(peerRefreshTimer);
  const errEl = $('loginErr');
  if (msg) {
    errEl.innerHTML = `${icon('alert')}<span>${esc(msg)}</span>`;
    errEl.hidden = false;
  } else {
    errEl.hidden = true;
  }
}

function showApp() {
  $('loginPage').hidden = true;
  $('app').hidden = false;
}

$('loginForm').addEventListener('submit', async (e) => {
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

$('logoutbtn').onclick = async () => {
  await api('/admin/logout', { method: 'POST' }).catch(() => {});
  showLogin();
  toast('已安全退出', 'info');
};

/* ---------- Stats & Summary ---------- */
async function refreshStats() {
  try {
    const s = await api('/admin/stats');
    if (s.protocol) {
      $('sbVer').textContent = `v${s.protocol} · ${s.client_version || 'dev'}`;
    }
    $('sbSeq').textContent = s.max_seq ?? 0;
    $('sbDb').textContent = `${((s.db_bytes || 0) / 1024).toFixed(0)} KiB`;

    const onlineStr = `${s.peers_online || 0} / ${s.peers_total || 0}`;
    $('onlineText').textContent = `${onlineStr} 在线`;
    $('stOnline').innerHTML = `${s.peers_online || 0} <small>/ ${s.peers_total || 0}</small>`;
    $('navOnline').textContent = `${s.peers_online || 0}`;

    const pending = s.pending_approvals || 0;
    $('stPending').textContent = pending;
    const navPending = $('navPending');
    if (pending > 0) {
      navPending.textContent = pending;
      navPending.hidden = false;
    } else {
      navPending.hidden = true;
    }

    $('stThreads').textContent = s.threads_total || 0;
    $('stTokens').textContent = s.tokens_active || 0;

    // 指挥台 lives in the templ console; this nav entry just carries its badge.
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

/* =========================================================================
   Tab 1: 成员 (Members)
   ========================================================================= */
const peerState = {
  page: 1,
  pageSize: 20,
  filter: '',
  type: '',
  q: '',
  cache: [],
};

const statusBadge = (st) => {
  const cls = st === 'active' ? 'ok' : (st === 'suspended' || st === 'failed' ? 'bad' : 'warn');
  const ic = st === 'active' ? 'ok' : (st === 'suspended' ? 'ban' : 'alert');
  return `<span class="badge ${cls}">${icon(ic, 'xs')}${esc(st)}</span>`;
};

const promptBadge = (p) => {
  const v = p.prompt_version ?? 0;
  if (!serverPromptVersion || v >= serverPromptVersion) {
    return `<span class="badge ok mono" title="指令版本">v${v}</span>`;
  }
  const when = p.prompt_updated_at ? fmtTime(p.prompt_updated_at) : '从未';
  return `<span class="badge warn mono" title="确认于 ${esc(when)} → 最新 v${serverPromptVersion}">v${v} → v${serverPromptVersion}</span>`;
};

// Memory reconciliation (§8.6): the assistant's own report of which stale
// relay rules it purged after an upgrade. Missing/older than the prompt it
// runs means old rules may still be competing with the current text.
const memoryBadge = (p) => {
  const mv = p.memory_version ?? 0;
  const pv = p.prompt_version ?? 0;
  if (!mv) {
    return pv ? '<span class="badge warn" title="尚未上报 memory 清理（旧规则可能仍残留）">mem ?</span>' : '';
  }
  const when = p.memory_reconciled_at ? fmtTime(p.memory_reconciled_at) : '未知';
  const tip = `v${mv} · ${when}\n${p.memory_note || ''}`;
  const cls = mv >= pv ? 'ok' : 'warn';
  return `<span class="badge ${cls} mono" title="${esc(tip)}">mem v${mv}</span>`;
};

const clientBadge = (p) => {
  const v = p.client_version || '';
  if (!v) return '<span class="muted xs">—</span>';
  // Update predicate is the receiver *revision* (DESIGN §8.9): the release tag
  // moves on every release, the rev only when the receiver code changed. So a
  // tag mismatch with an equal rev is not "needs update".
  const rev = p.client_rev || '';
  const when = p.client_updated_at ? fmtTime(p.client_updated_at) : '未知';
  if (rev && serverReceiverRev) {
    if (rev === serverReceiverRev) {
      return `<span class="badge ok mono" title="接收端与服务端同源码版本（${esc(v)}）">${esc(v)} <em class="muted">rev ${esc(rev)}</em></span>`;
    }
    return `<span class="badge warn mono" title="上报于 ${esc(when)} · rev ${esc(rev)} → 最新 rev ${esc(serverReceiverRev)}（${esc(serverClientVersion)}）">${esc(v)} → 新版</span>`;
  }
  // No rev reported (old binary or shell script): fall back to the tag.
  if (!rev && (!serverClientVersion || v === serverClientVersion)) {
    return `<span class="badge ok mono" title="客户端版本（未上报 rev）">${esc(v)}</span>`;
  }
  if (!rev) {
    return `<span class="badge warn mono" title="上报于 ${esc(when)} → 最新 ${esc(serverClientVersion)}（未上报 rev）">${esc(v)} → 新版</span>`;
  }
  return `<span class="badge warn mono" title="上报于 ${esc(when)} · rev ${esc(rev)}">${esc(v)}</span>`;
};

const transportBadge = (p) => {
  const isSSE = (p.transport || 'poll') === 'sse';
  return isSSE
    ? `<span class="badge ok">${icon('zap', 'xs')}SSE</span>`
    : `<span class="badge">${icon('radio', 'xs')}轮询</span>`;
};

const capsBadges = (c) => {
  if (!c || typeof c !== 'object') return '<span class="muted xs">—</span>';
  const keys = Object.keys(c).filter((k) => c[k]);
  if (!keys.length) return '<span class="muted xs">—</span>';
  return `<div class="chips">${keys.map((k) => `<span class="badge xs">${esc(k)}</span>`).join('')}</div>`;
};

async function refreshPeers() {
  const tbody = $('peerTable').querySelector('tbody');
  tbody.innerHTML = `<tr class="skel"><td colspan="8"><i></i></td></tr>`;

  try {
    const params = new URLSearchParams({
      page: peerState.page,
      page_size: peerState.pageSize,
    });
    if (peerState.filter) {
      if (peerState.filter === 'online') params.set('online', '1');
      else if (peerState.filter === 'pending') params.set('status', 'pending,verifying');
      else params.set('status', peerState.filter);
    }
    if (peerState.type) params.set('type', peerState.type);
    if (peerState.q) params.set('q', peerState.q);

    const r = await api('/admin/peers?' + params.toString());
    const peers = r.peers || [];
    peerState.cache = peers;

    // Update facet counts
    const fc = r.facets || {};
    $('fcAll').textContent = fc.all ?? '';
    $('fcOnline').textContent = fc.online ?? '';
    $('fcActive').textContent = fc['status:active'] ?? '';
    $('fcPending').textContent = (fc['status:pending'] || 0) + (fc['status:verifying'] || 0);
    $('fcSuspended').textContent = fc['status:suspended'] ?? '';

    // Update type select dropdown
    const typeSel = $('peerType');
    const curType = peerState.type;
    const typeCounts = r.types || {};
    typeSel.innerHTML = `<option value="">全部类型 (${fc.all || 0})</option>` +
      Object.keys(typeCounts).map((t) =>
        `<option value="${esc(t)}" ${t === curType ? 'selected' : ''}>${esc(t)} (${typeCounts[t]})</option>`
      ).join('');

    // Update datalist for peer picker inputs
    const dl = $('peerList');
    if (dl) {
      dl.innerHTML = peers.map((p) => `<option value="${esc(p.id)}">${esc(p.display_name || p.id)}</option>`).join('');
    }

    if (!peers.length) {
      tbody.innerHTML = `<tr><td colspan="8" class="empty-td"><div class="empty">${icon('users')}<p>没有找到匹配的成员</p></div></td></tr>`;
      renderPager($('peerPager'), { total: 0 });
      return;
    }

    tbody.innerHTML = peers.map((p) => {
      const offlineDesc = p.online ? '在线' : ('离线 ' + (p.offline_secs ? Math.floor(p.offline_secs / 60) + 'm' : ''));
      return `<tr>
        <td>
          <div class="who">
            ${avatar(p.id)}
            <div class="who-text">
              <b>${esc(p.id)}</b>
              <small>${esc(p.display_name || p.agent_type || '未命名')}</small>
            </div>
          </div>
        </td>
        <td>${statusBadge(p.status)}</td>
        <td>
          <div class="online-cell">
            <span><span class="dot ${p.online ? 'on' : 'off'}"></span>${p.online ? '在线' : '离线'}</span>
            <small title="${fmtTime(p.last_seen)}">${esc(ago(p.last_seen))}</small>
          </div>
        </td>
        <td>${transportBadge(p)}</td>
        <td>
          <div class="ver-stack">
            <span class="badge mono" title="协议版本">p${p.protocol_version ?? '?'}</span>
            ${promptBadge(p)}
            ${memoryBadge(p)}
            ${clientBadge(p)}
          </div>
        </td>
        <td>${capsBadges(p.capabilities)}</td>
        <td>
          <div class="profile" title="点击展开/收起">${esc(p.profile || '未填写自述能力')}</div>
        </td>
        <td class="td-actions">
          <div class="row-btns tight">
            ${p.status === 'suspended'
              ? `<button class="btn-icon ok" data-act="activate" data-id="${esc(p.id)}" data-tip="激活解封">${icon('play')}</button>`
              : `<button class="btn-icon warn" data-act="suspend" data-id="${esc(p.id)}" data-tip="暂停服务">${icon('pause')}</button>`}
            <button class="btn-icon danger" data-act="del" data-id="${esc(p.id)}" data-tip="删除身份">${icon('trash')}</button>
          </div>
        </td>
      </tr>`;
    }).join('');

    // Hook expand profile
    tbody.querySelectorAll('.profile').forEach((el) => {
      el.onclick = () => el.classList.toggle('open');
    });

    // Hook row actions
    tbody.querySelectorAll('button[data-act]').forEach((btn) => {
      btn.onclick = async (e) => {
        e.stopPropagation();
        const id = btn.dataset.id;
        const act = btn.dataset.act;
        if (act === 'del') {
          const ok = await confirmAction({
            title: `删除成员 ${id}？`,
            body: `删除后该成员名下的 Token 将全部撤销，该助手将立即失联。\n消息历史与审计记录将继续保留。`,
            danger: true,
            okText: '确认删除',
          });
          if (!ok) return;
          try {
            await api('/admin/peers/' + encodeURIComponent(id), { method: 'DELETE' });
            toast(`已删除成员 ${id}`, 'ok');
            refreshPeers();
            refreshStats();
          } catch (err) {
            toast('删除失败：' + err.message, 'bad');
          }
          return;
        }

        const target = act === 'suspend' ? 'suspended' : 'active';
        try {
          await api('/admin/peers/' + encodeURIComponent(id), {
            method: 'PATCH',
            body: JSON.stringify({ status: target }),
          });
          toast(`已更新 ${id} 状态为 ${target}`, 'ok');
          refreshPeers();
        } catch (err) {
          toast('操作失败：' + err.message, 'bad');
        }
      };
    });

    renderPager($('peerPager'), {
      total: r.total || peers.length,
      page: peerState.page,
      pageSize: peerState.pageSize,
      onPage: (p) => { peerState.page = p; refreshPeers(); },
      onPageSize: (s) => { peerState.pageSize = s; peerState.page = 1; refreshPeers(); },
    });

  } catch (err) {
    if (err.status !== 401) {
      tbody.innerHTML = `<tr><td colspan="8" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
    }
  }
}

// Peer filters
$('peerSeg').querySelectorAll('button').forEach((b) => {
  b.onclick = () => {
    $('peerSeg').querySelectorAll('button').forEach((x) => x.classList.remove('on'));
    b.classList.add('on');
    peerState.filter = b.dataset.v;
    peerState.page = 1;
    refreshPeers();
  };
});
$('peerType').onchange = (e) => {
  peerState.type = e.target.value;
  peerState.page = 1;
  refreshPeers();
};
let peerQTimer = null;
$('peerQ').oninput = (e) => {
  clearTimeout(peerQTimer);
  peerQTimer = setTimeout(() => {
    peerState.q = e.target.value.trim();
    peerState.page = 1;
    refreshPeers();
  }, 260);
};
$('refreshPeers').onclick = refreshPeers;

$('peerAuto').onchange = (e) => {
  if (e.target.checked) {
    peerRefreshTimer = setInterval(() => {
      if (currentRoute === 'members' && !$('app').hidden) {
        refreshPeers();
        refreshStats();
      }
    }, 15000);
    toast('已开启自动刷新 (15s)', 'info', 2000);
  } else {
    clearInterval(peerRefreshTimer);
  }
};

/* =========================================================================
   Tab 2: 消息 & 线程 (Threads & Messages)
   ========================================================================= */
const threadState = {
  mode: 'threads', // 'threads' | 'search'
  state: '',       // '' | 'held' | 'fused'
  q: '',
  page: 1,
  pageSize: 20,
};

async function refreshThreads() {
  if (threadState.mode === 'search') {
    refreshSearch();
    return;
  }
  const tbody = $('threadTable').querySelector('tbody');
  tbody.innerHTML = `<tr class="skel"><td colspan="6"><i></i></td></tr>`;

  try {
    const params = new URLSearchParams({
      page: threadState.page,
      page_size: threadState.pageSize,
    });
    if (threadState.state) params.set('state', threadState.state);
    if (threadState.q) params.set('filter', threadState.q);

    const r = await api('/admin/messages?' + params.toString());
    const threads = r.threads || [];
    const fuseLimit = r.fuse_max_messages || 200;

    if (!threads.length) {
      tbody.innerHTML = `<tr><td colspan="6" class="empty-td"><div class="empty">${icon('message')}<p>暂无匹配的消息线程</p></div></td></tr>`;
      renderPager($('threadPager'), { total: 0 });
      return;
    }

    tbody.innerHTML = threads.map((t) => {
      const parts = t.participants || [];
      const pct = Math.min(100, Math.round(((t.since_reset || t.count) / fuseLimit) * 100));
      const tone = t.fused ? 'bad' : (pct >= 80 ? 'warn' : 'brand');

      return `<tr class="clickable" data-root="${esc(t.root_id)}">
        <td>
          <div class="thread-cell">
            <code>${esc(t.root_id)}</code>
            <small title="${esc(t.last_preview)}">${esc(t.last_preview || '（空载荷）')}</small>
          </div>
        </td>
        <td>
          <div class="avatar-stack" title="${esc(parts.join(', '))}">
            ${parts.slice(0, 3).map((p) => avatar(p, 'sm')).join('')}
            ${parts.length > 3 ? `<span class="more">+${parts.length - 3}</span>` : ''}
          </div>
        </td>
        <td>
          <div class="meter ${tone}">
            <b>${t.count} 条 <small class="muted">(${t.since_reset ?? t.count}/${fuseLimit})</small></b>
            <i style="--p: ${pct}%; --tone: var(--${tone})"></i>
          </div>
        </td>
        <td>
          <div class="chips">
            ${t.held ? `<span class="badge warn">${icon('shield', 'xs')}待审 ${t.held}</span>` : ''}
            ${t.fused ? `<span class="badge bad">${icon('alert', 'xs')}熔断</span>` : ''}
            ${!t.held && !t.fused ? `<span class="badge ok">${icon('ok', 'xs')}正常</span>` : ''}
          </div>
        </td>
        <td>
          <div class="time-cell">
            <span>${esc(ago(t.last_at))}</span>
            <small class="muted">${fmtTime(t.last_at)}</small>
          </div>
        </td>
        <td class="td-actions">
          <div class="row-btns tight">
            <button class="btn-icon" data-view="${esc(t.root_id)}" data-tip="查看详情">${icon('eye')}</button>
            <button class="btn-icon warn" data-fuse="${esc(t.root_id)}" data-tip="Reset 熔断">${icon('rotate')}</button>
          </div>
        </td>
      </tr>`;
    }).join('');

    // Row click opens drawer
    tbody.querySelectorAll('tr[data-root]').forEach((tr) => {
      tr.onclick = (e) => {
        if (e.target.closest('button')) return;
        viewThread(tr.dataset.root);
      };
    });

    tbody.querySelectorAll('button[data-view]').forEach((b) => {
      b.onclick = () => viewThread(b.dataset.view);
    });

    tbody.querySelectorAll('button[data-fuse]').forEach((b) => {
      b.onclick = async () => {
        const root = b.dataset.fuse;
        const ok = await confirmAction({
          title: `重置线程熔断？`,
          body: `线程: ${root}\n将重置熔断计数器（历史消息完整保留，从当前序号重新计费）。`,
          danger: false,
          okText: '确认重置',
        });
        if (!ok) return;
        try {
          await api('/admin/fuse/reset', { method: 'POST', body: JSON.stringify({ root_id: root }) });
          toast(`已重置线程 ${root} 的熔断状态`, 'ok');
          refreshThreads();
        } catch (err) {
          toast('重置失败：' + err.message, 'bad');
        }
      };
    });

    renderPager($('threadPager'), {
      total: r.total || threads.length,
      page: threadState.page,
      pageSize: threadState.pageSize,
      onPage: (p) => { threadState.page = p; refreshThreads(); },
      onPageSize: (s) => { threadState.pageSize = s; threadState.page = 1; refreshThreads(); },
    });

  } catch (err) {
    if (err.status !== 401) {
      tbody.innerHTML = `<tr><td colspan="6" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
    }
  }
}

async function refreshSearch() {
  const container = $('searchList');
  if (!threadState.q) {
    container.innerHTML = `<div class="empty">${icon('search')}<p>请输入关键字搜索消息 Payload</p></div>`;
    renderPager($('threadPager'), { total: 0 });
    return;
  }
  container.innerHTML = `<div class="empty"><p class="muted">正在搜索…</p></div>`;

  try {
    const params = new URLSearchParams({
      q: threadState.q,
      page: threadState.page,
      page_size: threadState.pageSize,
    });
    const r = await api('/admin/messages?' + params.toString());
    const items = r.items || [];

    if (!items.length) {
      container.innerHTML = `<div class="empty">${icon('search')}<p>未找到匹配 "${esc(threadState.q)}" 的消息</p></div>`;
      renderPager($('threadPager'), { total: 0 });
      return;
    }

    items.forEach((m) => msgPayloadMap.set(m.seq, m.payload));

    container.innerHTML = items.map((m) => {
      const isPending = m.approval_state === 'pending';
      return `<div class="msg ${isPending ? 'held' : ''}">
        <div class="msg-head">
          <span class="num">#${m.seq}</span>
          ${avatar(m.sender, 'sm')}
          <b>${esc(m.sender)}</b>
          <span>→</span>
          ${avatar(m.recipient, 'sm')}
          <b>${esc(m.recipient)}</b>
          <span class="badge mono xs">${esc(m.kind)}</span>
          ${isPending ? `<span class="badge warn">${icon('shield', 'xs')}待审批</span>` : ''}
          <span class="sp"></span>
          <span class="muted xs" title="${fmtTime(m.created_at)}">${esc(ago(m.created_at))}</span>
          <button class="btn btn-ghost btn-xs" data-copy-msg="${m.seq}" title="复制消息内容">${icon('copy', 'xs')}复制</button>
          <button class="btn btn-ghost btn-xs" onclick="window.__viewThread('${esc(m.thread)}')">${icon('eye', 'xs')}查看线程</button>
        </div>
        <div class="msg-body clamp">${esc(m.payload)}</div>
        <div class="msg-foot">
          <span>thread: <code>${esc(m.thread)}</code></span>
          <span>id: <code>${esc(m.id)}</code></span>
        </div>
      </div>`;
    }).join('');

    renderPager($('threadPager'), {
      total: r.total || items.length,
      page: threadState.page,
      pageSize: threadState.pageSize,
      onPage: (p) => { threadState.page = p; refreshSearch(); },
      onPageSize: (s) => { threadState.pageSize = s; threadState.page = 1; refreshSearch(); },
    });
  } catch (err) {
    container.innerHTML = `<div class="empty bad">${icon('err')}<p>搜索失败：${esc(err.message)}</p></div>`;
  }
}

// Handshake rendering inside thread drawer
const renderHandshakeBadge = (m) => {
  if (m.kind === 'status' && m.status) {
    const cls = m.status === 'blocked' ? 'bad' : (m.status === 'cancelled' ? 'warn' : 'ok');
    return `<div class="chips"><span class="badge ${cls}">${icon('activity', 'xs')}status: ${esc(m.status)}</span>${m.in_reply_to ? `<span class="muted xs">re ${esc(m.in_reply_to)}</span>` : ''}</div>`;
  }
  if (m.kind === 'permission_request') {
    const exp = m.expires_at ? fmtTime(m.expires_at) : '-';
    return `<div class="chips"><span class="badge warn">${icon('shield', 'xs')}permission_request</span><span class="muted xs">过期 ${esc(exp)}</span></div>`;
  }
  if (m.kind === 'permission_decision' && m.decision) {
    const cls = m.decision === 'allow' ? 'ok' : 'bad';
    const ic = m.decision === 'allow' ? 'ok' : 'ban';
    return `<div class="chips"><span class="badge ${cls}">${icon(ic, 'xs')}decision: ${esc(m.decision)}</span>${m.in_reply_to ? `<span class="muted xs">re ${esc(m.in_reply_to)}</span>` : ''}</div>`;
  }
  return '';
};

const renderHandshakeFields = (m) => {
  if (m.kind !== 'permission_request') return '';
  const rows = [['op', m.op], ['target', m.target], ['detail', m.detail]]
    .filter(([, v]) => v)
    .map(([k, v]) => `<div><span class="muted xs">${k}:</span> <code>${esc(v)}</code></div>`).join('');
  if (!rows) return '';
  return `<div class="perm-fields">${rows}<div class="note">${icon('alert', 'xs')}只信赖以上结构化字段；下方载荷人话来自发起方，仅供参考。</div></div>`;
};

async function viewThread(root) {
  const drawer = $('threadDrawer');
  $('drawerTitle').textContent = root;
  $('drawerTitle').title = root;
  $('drawerMeta').textContent = '加载中…';
  const body = $('drawerBody');
  body.innerHTML = `<div class="empty"><p class="muted">加载会话历史…</p></div>`;

  setupDialog(drawer);
  drawer.showModal();

  const copyThreadExport = async (format) => {
    const dropdown = $('copyDropdown');
    if (dropdown) dropdown.open = false;
    const summary = $('copySummary');
    const origHtml = summary ? summary.innerHTML : '';
    try {
      let text = '';
      try {
        const res = await fetch('/admin/messages/export?thread=' + encodeURIComponent(root) + '&format=' + format);
        if (res.ok) {
          text = await res.text();
        }
      } catch (_) {}
      if (!text && Array.isArray(window.__currentThreadItems)) {
        if (format === 'jsonl') {
          text = window.__currentThreadItems.map((m) => JSON.stringify(m)).join('\n') + '\n';
        } else {
          text = `# thread ${root}\n\n` + window.__currentThreadItems.map((m) =>
            `## #${m.seq} ${m.sender} → ${m.recipient} (${m.kind}, ${m.approval_state || 'n/a'})\n\n${m.payload || ''}\n\n`
          ).join('');
        }
      }
      if (!text) {
        toast('该线程无消息记录', 'info');
        return;
      }
      const ok = await copyToClipboard(text);
      if (ok) {
        toast(`已复制 ${format === 'jsonl' ? 'JSONL' : 'Markdown'} 到剪贴板`, 'ok', 1800);
        if (summary) {
          summary.innerHTML = `${icon('check', 'xs')} 已复制<svg class="icon xs chev"><use href="#i-chev-d"/></svg>`;
          setTimeout(() => { if (summary) summary.innerHTML = origHtml; }, 1600);
        }
      } else {
        toast('复制失败，请重试', 'bad');
      }
    } catch (err) {
      toast('复制失败: ' + err.message, 'bad');
    }
  };

  $('copyMd').onclick = () => copyThreadExport('markdown');
  $('copyJsonl').onclick = () => copyThreadExport('jsonl');
  $('expMd').onclick = () => {
    $('expDropdown').open = false;
    window.location = '/admin/messages/export?thread=' + encodeURIComponent(root) + '&format=markdown';
  };
  $('expJsonl').onclick = () => {
    $('expDropdown').open = false;
    window.location = '/admin/messages/export?thread=' + encodeURIComponent(root) + '&format=jsonl';
  };

  try {
    const r = await api('/admin/messages?thread=' + encodeURIComponent(root));
    const items = r.items || [];
    window.__currentThreadItems = items;
    items.forEach((m) => msgPayloadMap.set(m.seq, m.payload));
    $('drawerMeta').textContent = `共 ${items.length} 条消息 · 参与者: ${[...new Set(items.flatMap((m) => [m.sender, m.recipient]))].filter((x) => x && x !== '*').join(', ')}`;

    if (!items.length) {
      body.innerHTML = `<div class="empty">${icon('message')}<p>该线程无消息记录</p></div>`;
      return;
    }

    body.innerHTML = `<div class="timeline">${items.map((m) => {
      const isPending = m.approval_state === 'pending';
      return `<div class="msg ${isPending ? 'held' : ''}">
        <div class="msg-head">
          <span class="num">#${m.seq}</span>
          ${avatar(m.sender, 'sm')}
          <b>${esc(m.sender)}</b>
          <span>→</span>
          ${avatar(m.recipient, 'sm')}
          <b>${esc(m.recipient)}</b>
          <span class="badge mono xs">${esc(m.kind)}</span>
          <span class="sp"></span>
          <span class="muted xs" title="${fmtTime(m.created_at)}">${esc(ago(m.created_at))}</span>
          <button class="btn btn-ghost btn-xs" data-copy-msg="${m.seq}" title="复制消息内容">${icon('copy', 'xs')}复制</button>
        </div>
        ${renderHandshakeBadge(m)}
        ${renderHandshakeFields(m)}
        <div class="msg-body">${esc(m.payload)}</div>
        <div class="msg-foot">
          <span>id: <code>${esc(m.id)}</code></span>
          <span>ack: ${m.acked_count || 0}</span>
          ${m.approval_state && m.approval_state !== 'n/a' ? `<span>审批状态: <b>${esc(m.approval_state)}</b></span>` : ''}
        </div>
        ${isPending ? `
          <div class="alert alert-warn">
            ${icon('shield')}
            <div style="flex:1">
              <b>此消息需要管理员审批放行</b>
              <p class="muted xs">批准后将推入队列由接收方消费，拒绝则丢弃。</p>
            </div>
            <div class="row-btns tight">
              <button class="btn btn-primary btn-sm" data-approve="1" data-seq="${m.seq}">${icon('check')}批准</button>
              <button class="btn btn-outline btn-sm" data-approve="0" data-seq="${m.seq}">${icon('ban')}拒绝</button>
            </div>
          </div>
        ` : ''}
      </div>`;
    }).join('')}</div>`;

    body.querySelectorAll('button[data-approve]').forEach((btn) => {
      btn.onclick = async () => {
        const seq = Number(btn.dataset.seq);
        const approve = btn.dataset.approve === '1';
        try {
          await api('/admin/messages/approve', {
            method: 'POST',
            body: JSON.stringify({ seq, approve }),
          });
          toast(approve ? '已批准放行' : '已拒绝该消息', approve ? 'ok' : 'info');
          viewThread(root);
          refreshThreads();
          refreshStats();
        } catch (err) {
          toast('审批失败：' + err.message, 'bad');
        }
      };
    });

  } catch (err) {
    body.innerHTML = `<div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div>`;
  }
}
window.__viewThread = viewThread;

// Thread view toggles
$('threadMode').querySelectorAll('button').forEach((b) => {
  b.onclick = () => {
    $('threadMode').querySelectorAll('button').forEach((x) => x.classList.remove('on'));
    b.classList.add('on');
    threadState.mode = b.dataset.v;
    threadState.page = 1;
    const isSearch = threadState.mode === 'search';
    $('threadListView').hidden = isSearch;
    $('searchView').hidden = !isSearch;
    $('threadState').hidden = isSearch;
    $('threadQ').placeholder = isSearch ? '搜索 Payload 全文内容…' : '按 root_id / 参与者过滤';
    refreshThreads();
  };
});

$('threadState').querySelectorAll('button').forEach((b) => {
  b.onclick = () => {
    $('threadState').querySelectorAll('button').forEach((x) => x.classList.remove('on'));
    b.classList.add('on');
    threadState.state = b.dataset.v;
    threadState.page = 1;
    refreshThreads();
  };
});

$('threadSearchForm').onsubmit = (e) => {
  e.preventDefault();
  threadState.q = $('threadQ').value.trim();
  threadState.page = 1;
  refreshThreads();
};

let threadQTimer = null;
$('threadQ').oninput = (e) => {
  clearTimeout(threadQTimer);
  threadQTimer = setTimeout(() => {
    threadState.q = e.target.value.trim();
    threadState.page = 1;
    refreshThreads();
  }, 350);
};
$('refreshThreads').onclick = refreshThreads;

/* =========================================================================
   Tab 3: 邀请 & Prompt (Prompts & Onboarding)
   ========================================================================= */
$('genPrompt').onclick = async () => {
  const btn = $('genPrompt');
  const typeInput = document.querySelector('input[name="pType"]:checked');
  const agentType = typeInput ? typeInput.value : 'muse';
  const peerId = $('pPeer').value.trim();

  btn.disabled = true;
  btn.classList.add('loading');
  try {
    const r = await api('/admin/prompts', {
      method: 'POST',
      body: JSON.stringify({
        agent_type: agentType,
        peer_id: peerId,
        create_invite: true,
      }),
    });
    $('promptOut').hidden = false;
    $('inviteCode').textContent = r.code;
    $('promptText').textContent = r.prompt;
    toast('邀请码与 Onboarding Prompt 已生成', 'ok');
  } catch (e) {
    toast('生成失败: ' + e.message, 'bad');
  } finally {
    btn.disabled = false;
    btn.classList.remove('loading');
  }
};

$('genReconf').onclick = async () => {
  const peer = $('rPeer').value.trim();
  if (!peer) {
    toast('请先选择或输入成员身份', 'warn');
    $('rPeer').focus();
    return;
  }
  const btn = $('genReconf');
  btn.disabled = true;
  try {
    const r = await api('/admin/prompts', {
      method: 'POST',
      body: JSON.stringify({ agent_type: 'muse', peer_id: peer, reconfigure: true }),
    });
    $('reconfEmpty').hidden = true;
    $('reconfOut').hidden = false;
    $('reconfName').textContent = `${peer}-reconfigure.md`;
    $('reconfText').textContent =
      `# agent-relay 指令更新 · 身份: ${peer}\n# 沿用原 token，无需重新注册\n\n${r.prompt}`;
    toast(`已生成 ${peer} 的重配指令`, 'ok');
  } catch (e) {
    toast('生成重配失败: ' + e.message, 'bad');
  } finally {
    btn.disabled = false;
  }
};

// Copy & Download delegate
document.addEventListener('click', async (e) => {
  // Close any open dropdowns if clicking outside
  if (!e.target.closest('details.dropdown')) {
    document.querySelectorAll('details.dropdown[open]').forEach((d) => { d.open = false; });
  }

  const copyBtn = e.target.closest('[data-copy]');
  if (copyBtn) {
    const selector = copyBtn.dataset.copy;
    const target = document.querySelector(selector);
    if (target) {
      const ok = await copyToClipboard(target.textContent || target.value || '');
      if (ok) {
        toast('已复制到剪贴板', 'ok', 1800);
        const origHtml = copyBtn.innerHTML;
        copyBtn.innerHTML = `${icon('check', 'xs')} 已复制`;
        setTimeout(() => { copyBtn.innerHTML = origHtml; }, 1600);
      } else {
        toast('复制失败，请重试', 'bad');
      }
    }
    return;
  }
  const copyMsgBtn = e.target.closest('[data-copy-msg]');
  if (copyMsgBtn) {
    const seq = Number(copyMsgBtn.dataset.copyMsg);
    let text = msgPayloadMap.get(seq);
    if (text === undefined) {
      const msgEl = copyMsgBtn.closest('.msg');
      const bodyEl = msgEl && msgEl.querySelector('.msg-body');
      text = bodyEl ? bodyEl.textContent : '';
    }
    const ok = await copyToClipboard(text);
    if (ok) {
      toast(seq ? `已复制消息 #${seq} 内容` : '已复制消息内容', 'ok', 1800);
      const origHtml = copyMsgBtn.innerHTML;
      copyMsgBtn.innerHTML = `${icon('check', 'xs')} 已复制`;
      setTimeout(() => { copyMsgBtn.innerHTML = origHtml; }, 1600);
    } else {
      toast('复制失败，请重试', 'bad');
    }
    return;
  }
  const copyThreadIdBtn = e.target.closest('#copyThreadId');
  if (copyThreadIdBtn) {
    const threadId = $('drawerTitle').textContent.trim();
    if (threadId) {
      const ok = await copyToClipboard(threadId);
      if (ok) {
        toast('已复制线程 ID 到剪贴板', 'ok', 1800);
        const origHtml = copyThreadIdBtn.innerHTML;
        copyThreadIdBtn.innerHTML = `${icon('check', 'xs')}`;
        setTimeout(() => { copyThreadIdBtn.innerHTML = origHtml; }, 1600);
      }
    }
    return;
  }
  const dlBtn = e.target.closest('[data-dl]');
  if (dlBtn) {
    const target = document.querySelector(dlBtn.dataset.dl);
    const fname = dlBtn.dataset.name || 'download.txt';
    if (target) {
      const blob = new Blob([target.textContent || ''], { type: 'text/markdown;charset=utf-8' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = fname;
      a.click();
      toast(`已下载 ${fname}`, 'ok', 1800);
    }
  }
});

/* =========================================================================
   Tab 4: 审计日志 (Audit Log)
   ========================================================================= */
const auditState = {
  page: 1,
  pageSize: 20,
  actor: '',
  action: '',
  q: '',
  cache: [],
};

async function refreshAudit() {
  const tbody = $('auditTable').querySelector('tbody');
  tbody.innerHTML = `<tr class="skel"><td colspan="5"><i></i></td></tr>`;

  try {
    const params = new URLSearchParams({
      page: auditState.page,
      page_size: auditState.pageSize,
      facets: '1',
    });
    if (auditState.actor) params.set('actor', auditState.actor);
    if (auditState.action) params.set('action', auditState.action);
    if (auditState.q) params.set('q', auditState.q);

    const r = await api('/admin/audit?' + params.toString());
    const entries = r.entries || [];
    auditState.cache = entries;

    // Populate facet dropdowns (selection is preserved via `selected`).
    if (Array.isArray(r.actors)) {
      const curActor = auditState.actor;
      $('aActor').innerHTML = `<option value="">全部 actor</option>` +
        r.actors.map((a) => `<option value="${esc(a)}" ${a === curActor ? 'selected' : ''}>${esc(a)}</option>`).join('');
    }
    if (Array.isArray(r.actions)) {
      const curAction = auditState.action;
      $('aAction').innerHTML = `<option value="">全部 action</option>` +
        r.actions.map((a) => `<option value="${esc(a)}" ${a === curAction ? 'selected' : ''}>${esc(a)}</option>`).join('');
    }

    if (!entries.length) {
      tbody.innerHTML = `<tr><td colspan="5" class="empty-td"><div class="empty">${icon('scroll')}<p>暂无审计记录</p></div></td></tr>`;
      renderPager($('auditPager'), { total: 0 });
      return;
    }

    tbody.innerHTML = entries.map((e) => {
      let tone = 'muted';
      if (e.action.includes('error') || e.action.includes('fail') || e.action.includes('tripped') || e.action.includes('deleted')) tone = 'bad';
      else if (e.action.includes('verify') || e.action.includes('approve') || e.action.includes('rotate')) tone = 'ok';
      else if (e.action.includes('status') || e.action.includes('requested')) tone = 'warn';

      return `<tr>
        <td class="col-seq"><span class="num">#${e.seq}</span></td>
        <td>
          <div class="time-cell">
            <span>${fmtTime(e.ts)}</span>
            <small>${esc(ago(e.ts))}</small>
          </div>
        </td>
        <td>
          <div class="who">
            ${avatar(e.actor, 'sm')}
            <b>${esc(e.actor)}</b>
          </div>
        </td>
        <td>
          <span class="badge ${tone} mono">${esc(e.action)}</span>
        </td>
        <td>
          <div class="kv-detail">${esc(e.detail || '—')}</div>
        </td>
      </tr>`;
    }).join('');

    renderPager($('auditPager'), {
      total: r.total || entries.length,
      page: auditState.page,
      pageSize: auditState.pageSize,
      onPage: (p) => { auditState.page = p; refreshAudit(); },
      onPageSize: (s) => { auditState.pageSize = s; auditState.page = 1; refreshAudit(); },
    });

  } catch (err) {
    if (err.status !== 401) {
      tbody.innerHTML = `<tr><td colspan="5" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
    }
  }
}

$('aActor').onchange = (e) => {
  auditState.actor = e.target.value;
  auditState.page = 1;
  refreshAudit();
};
$('aAction').onchange = (e) => {
  auditState.action = e.target.value;
  auditState.page = 1;
  refreshAudit();
};
let aQTimer = null;
$('aQ').oninput = (e) => {
  clearTimeout(aQTimer);
  aQTimer = setTimeout(() => {
    auditState.q = e.target.value.trim();
    auditState.page = 1;
    refreshAudit();
  }, 300);
};
$('refreshAudit').onclick = refreshAudit;
$('exportAudit').onclick = () => {
  const blob = new Blob([JSON.stringify(auditState.cache, null, 2)], { type: 'application/json' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = `audit-${Date.now()}.json`;
  a.click();
  toast('已导出审计记录', 'ok');
};

/* =========================================================================
   Tab 5: Token 管理 (Tokens)
   ========================================================================= */
const tokenState = {
  page: 1,
  pageSize: 20,
  state: '', // '' | 'active' | 'revoked'
  q: '',
};

async function refreshTokens() {
  const tbody = $('tokenTable').querySelector('tbody');
  tbody.innerHTML = `<tr class="skel"><td colspan="8"><i></i></td></tr>`;

  try {
    const params = new URLSearchParams({
      page: tokenState.page,
      page_size: tokenState.pageSize,
    });
    if (tokenState.state) params.set('state', tokenState.state);
    if (tokenState.q) params.set('q', tokenState.q);

    const r = await api('/admin/tokens?' + params.toString());
    const toks = r.tokens || [];

    if (!toks.length) {
      tbody.innerHTML = `<tr><td colspan="8" class="empty-td"><div class="empty">${icon('key')}<p>暂无匹配的 Token</p></div></td></tr>`;
      renderPager($('tokenPager'), { total: 0 });
      return;
    }

    tbody.innerHTML = toks.map((t) => {
      const isRevoked = !!t.revoked_at;
      return `<tr>
        <td class="col-seq"><span class="num">${t.id}</span></td>
        <td>
          <div class="who">
            ${avatar(t.peer_id, 'sm')}
            <b>${esc(t.peer_id)}</b>
          </div>
        </td>
        <td><code>${esc(t.hash_prefix)}…</code></td>
        <td><span class="badge xs">${esc(t.label || 'default')}</span></td>
        <td>
          <div class="time-cell">
            <span>${fmtTime(t.created_at)}</span>
            <small>${esc(ago(t.created_at))}</small>
          </div>
        </td>
        <td>
          <div class="time-cell">
            <span>${t.last_used_at ? fmtTime(t.last_used_at) : '从未'}</span>
            <small>${t.last_ip ? esc(t.last_ip) : '—'}</small>
          </div>
        </td>
        <td>
          ${isRevoked
            ? `<span class="badge bad">${icon('ban', 'xs')}已撤销</span>`
            : `<span class="badge ok">${icon('ok', 'xs')}有效</span>`}
        </td>
        <td class="td-actions">
          ${isRevoked
            ? `<span class="muted xs">—</span>`
            : `<button class="btn-icon danger" data-rev="${t.id}" data-peer="${esc(t.peer_id)}" data-tip="撤销此凭据">${icon('ban')}</button>`}
        </td>
      </tr>`;
    }).join('');

    tbody.querySelectorAll('button[data-rev]').forEach((b) => {
      b.onclick = async () => {
        const id = b.dataset.rev;
        const peer = b.dataset.peer;
        const ok = await confirmAction({
          title: `撤销 Token #${id}？`,
          body: `所属成员: ${peer}\n撤销后助手使用该 Token 的请求将被立即拒绝，无法再次恢复。`,
          danger: true,
          okText: '确认撤销',
        });
        if (!ok) return;
        try {
          await api('/admin/tokens/' + id, { method: 'DELETE' });
          toast(`已撤销 Token #${id}`, 'ok');
          refreshTokens();
          refreshStats();
        } catch (err) {
          toast('撤销失败：' + err.message, 'bad');
        }
      };
    });

    renderPager($('tokenPager'), {
      total: r.total || toks.length,
      page: tokenState.page,
      pageSize: tokenState.pageSize,
      onPage: (p) => { tokenState.page = p; refreshTokens(); },
      onPageSize: (s) => { tokenState.pageSize = s; tokenState.page = 1; refreshTokens(); },
    });

  } catch (err) {
    if (err.status !== 401) {
      tbody.innerHTML = `<tr><td colspan="8" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
    }
  }
}

$('tokSeg').querySelectorAll('button').forEach((b) => {
  b.onclick = () => {
    $('tokSeg').querySelectorAll('button').forEach((x) => x.classList.remove('on'));
    b.classList.add('on');
    tokenState.state = b.dataset.v;
    tokenState.page = 1;
    refreshTokens();
  };
});
let tokQTimer = null;
$('tokQ').oninput = (e) => {
  clearTimeout(tokQTimer);
  tokQTimer = setTimeout(() => {
    tokenState.q = e.target.value.trim();
    tokenState.page = 1;
    refreshTokens();
  }, 260);
};
$('refreshTokens').onclick = refreshTokens;

// Rotate Token Modal
const rotDlg = $('rotateDlg');
setupDialog(rotDlg);
$('openRotate').onclick = () => {
  $('rotPeer').value = '';
  $('rotLabel').value = '';
  $('rotResult').hidden = true;
  $('rotBtn').disabled = false;
  rotDlg.showModal();
};

$('rotForm').onsubmit = async (e) => {
  e.preventDefault();
  const peer = $('rotPeer').value.trim();
  const label = $('rotLabel').value.trim() || 'rotated';
  if (!peer) {
    toast('请填写成员身份 (peer id)', 'warn');
    return;
  }
  const btn = $('rotBtn');
  btn.disabled = true;
  try {
    const r = await api('/admin/tokens/rotate', {
      method: 'POST',
      body: JSON.stringify({ peer_id: peer, label }),
    });
    $('rotToken').textContent = r.token;
    $('rotResult').hidden = false;
    toast(`已成功为 ${peer} 签发新 Token`, 'ok');
    refreshTokens();
    refreshStats();
  } catch (err) {
    toast('签发失败：' + err.message, 'bad');
    btn.disabled = false;
  }
};

/* =========================================================================
   Tab 6: 系统 (System & Updates)
   ========================================================================= */
let updJobTimer = null;

async function refreshSystem() {
  // 1. Stats and Info
  try {
    const s = await api('/admin/stats');
    $('sysInfo').innerHTML = `
      <dt>${icon('hash', 'xs')} 最大消息序号 (max_seq)</dt><dd>${s.max_seq ?? '-'}</dd>
      <dt>${icon('db', 'xs')} SQLite 数据库体积</dt><dd>${((s.db_bytes || 0) / 1024).toFixed(0)} KiB</dd>
      <dt>${icon('package', 'xs')} 协议版本 (protocol)</dt><dd>v${s.protocol ?? '-'}</dd>
      <dt>${icon('terminal', 'xs')} 最低兼容客户端 (min_client)</dt><dd>v${s.min_client ?? '-'}</dd>
      <dt>${icon('wrench', 'xs')} 服务端指令版本</dt><dd>v${s.prompt_version ?? '-'}</dd>
      <dt>${icon('server', 'xs')} 官方客户端 Release</dt><dd>${esc(s.client_version || 'dev')}</dd>
      <dt>${icon('package', 'xs')} 接收端源码版本 (receiver_rev)</dt><dd>${esc(s.receiver_rev || 'dev（不催更新）')}</dd>
    `;
  } catch {}

  // 2. Retention & Guard Policy
  try {
    const c = await api('/admin/config');
    const r = c.retention || {};
    const g = c.guard || {};
    const p = c.presence || {};
    $('sysPolicy').innerHTML = `
      <dt>${icon('timer', 'xs')} 消息保留时长 (TTL)</dt><dd>${r.message_ttl_days} 天</dd>
      <dt>${icon('trash', 'xs')} 离线成员剪枝</dt><dd>${r.peer_prune_after_days} 天</dd>
      <dt>${icon('scroll', 'xs')} 审计日志保留</dt><dd>${r.audit_retention_days} 天</dd>
      <dt>${icon('alert', 'xs')} 会话熔断阈值</dt><dd>${g.fuse_max_messages} 条 / ${Math.floor((g.fuse_max_age_secs || 0) / 3600)} 小时</dd>
      <dt>${icon('activity', 'xs')} 发送限流速率</dt><dd>${g.rate_per_minute} 次 / 分钟</dd>
      <dt>${icon('radio', 'xs')} 在线判定超时</dt><dd>${p.online_timeout_secs} 秒</dd>
      <dt>${icon('shield', 'xs')} 冒烟验证超时</dt><dd>${p.verify_timeout_secs} 秒</dd>
    `;
  } catch (err) {
    $('sysPolicy').innerHTML = `<p class="muted xs">配置读取失败：${esc(err.message)}</p>`;
  }

  // 3. Update Status
  try {
    const s = await api('/admin/update/status');
    const c = s.current || {};
    $('updCurrent').innerHTML = `当前版本: <b>${esc(c.tag || c.version || '?')}</b> · protocol <b>${c.protocol ?? '?'}</b> · min_client <b>${c.min_client ?? '?'}</b> · 部署模式: <span class="badge xs">${esc(s.mode || '?')}</span>`;

    if (s.mode === 'docker') {
      $('updDockerCard').hidden = false;
      $('updApply').disabled = true;
      $('updDockerCmd').textContent =
        `# 拉取目标版本镜像并重建容器（数据卷与配置保持不动）：\n` +
        `docker pull ghcr.io/alixwang/agent-relay:<目标版本> && \\\n` +
        `docker rm -f agent-relay && \\\n` +
        `docker run -d --name agent-relay --restart unless-stopped \\\n` +
        `  -v /var/lib/agent-relay:/var/lib/agent-relay \\\n` +
        `  -v /etc/agent-relay/config.toml:/etc/agent-relay/config.toml:ro \\\n` +
        `  ghcr.io/alixwang/agent-relay:<目标版本>`;
    } else if (s.mode !== 'systemd') {
      $('updCurrent').innerHTML += ` <span class="muted xs">（非 systemd 环境，Web 自动更新已禁用）</span>`;
      $('updApply').disabled = true;
    }
    if (s.job) renderUpdJob(s.job);
  } catch (e) {
    $('updCurrent').textContent = '读取更新状态失败: ' + e.message;
  }
}

function renderUpdJob(j) {
  const st = j.status === 'ok' ? 'ok' : (j.status === 'running' ? 'warn' : 'bad');
  $('updLog').textContent = `[Job ${j.id}] 目标版本: ${j.version} → 状态: ${j.status}\n` + (j.log || '');
  $('updLog').dataset.state = st;
}

$('updCheck').onclick = async () => {
  const v = $('updVer').value.trim();
  $('updApply').disabled = true;
  $('updWarn').innerHTML = `<p class="muted xs">正在检查${v ? '指定版本 ' + esc(v) : ' GitHub 最新 Release'}…</p>`;
  try {
    const r = await api('/admin/update/check', { method: 'POST', body: JSON.stringify({ version: v }) });
    const ver = r.version;
    $('updVer').value = ver;
    const changed = r.protocol_change || r.min_client_change;
    if (r.unknown) {
      $('updWarn').innerHTML = `<div class="alert alert-warn">${icon('alert')}<span><b>${esc(ver)}</b> 元数据不可达：继续升级将被视为跨协议变更，必须勾选下方确认项。</span></div>`;
      $('updAckRow').hidden = false;
    } else if (changed) {
      $('updWarn').innerHTML = `<div class="alert alert-warn">${icon('alert')}<span><b>${esc(ver)}</b> 包含协议重大变更 (protocol_change=${r.protocol_change}, min_client_change=${r.min_client_change})：现有助手可能需要重新运行 Onboarding Prompt，必须勾选确认。</span></div>`;
      $('updAckRow').hidden = false;
    } else {
      $('updWarn').innerHTML = `<div class="alert alert-ok">${icon('ok')}<span><b>${esc(ver)}</b> 协议完全兼容，支持无缝热更新（包含自动备份、健康检查与失败回滚）。</span></div>`;
      $('updAckRow').hidden = true;
    }
    $('updApply').disabled = false;
    $('updApply').dataset.version = ver;
  } catch (e) {
    toast('检查更新失败: ' + e.message, 'bad');
    $('updWarn').innerHTML = `<div class="alert alert-bad">${icon('err')}<span>检查失败：${esc(e.message)}</span></div>`;
  }
};

$('updApply').onclick = async () => {
  const v = $('updApply').dataset.version;
  if (!v) return;
  if (!$('updAckRow').hidden && !$('updAck').checked) {
    toast('跨协议更新必须先勾选确认条款', 'warn');
    return;
  }
  const ok = await confirmAction({
    title: `应用版本更新到 ${v}？`,
    body: `服务将在后台下载二进制文件，验证 SHA256，备份当前运行文件并重启 systemd 单元。\n健康检查失败将自动回滚。`,
    danger: false,
    okText: '立即更新',
  });
  if (!ok) return;

  try {
    const r = await api('/admin/update/apply', {
      method: 'POST',
      body: JSON.stringify({ version: v, acknowledge_protocol_change: $('updAck').checked }),
    });
    $('updLog').textContent = `[Job ${r.job_id}] 升级任务已启动，正在轮询日志…\n`;
    clearInterval(updJobTimer);
    updJobTimer = setInterval(async () => {
      try {
        const s = await api('/admin/update/status');
        if (s.job) {
          renderUpdJob(s.job);
          if (s.job.status !== 'running') {
            clearInterval(updJobTimer);
            refreshSystem();
            if (s.job.status === 'ok') toast(`成功升级到 ${v}`, 'ok');
            else toast(`升级异常中断: ${s.job.status}`, 'bad');
          }
        }
      } catch {}
    }, 2500);
  } catch (e) {
    toast('升级触发失败: ' + e.message, 'bad');
  }
};

/* =========================================================================
   Initialization
   ========================================================================= */
(async () => {
  initTheme();
  initNavigation();

  // Initial check
  const loggedIn = await refreshStats();
  if (loggedIn) {
    const route = location.hash.replace(/^#/, '') || 'members';
    setRoute(route);
  } else {
    showLogin();
  }
})();
