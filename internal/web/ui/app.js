async function api(path, opts = {}) {
  const o = { headers: { 'Content-Type': 'application/json' }, ...opts };
  o.headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  const r = await fetch(path, o);
  let body = null;
  try { body = await r.json(); } catch { body = null; }
  if (!r.ok) throw new Error((body && body.error) || r.statusText);
  return body;
}
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
const ago = (ts) => {
  if (!ts) return '从未';
  const d = Math.floor(Date.now() / 1000 - ts);
  if (d < 60) return d + 's 前';
  if (d < 3600) return Math.floor(d / 60) + 'm 前';
  if (d < 86400) return Math.floor(d / 3600) + 'h 前';
  return Math.floor(d / 86400) + 'd 前';
};
const fmtTime = (ts) => ts ? new Date(ts * 1000).toLocaleString() : '-';
const statusBadge = (st) => {
  const cls = st === 'active' ? 'ok' : (st === 'suspended' || st === 'failed' ? 'bad' : 'warn');
  return `<span class="badge ${cls}">${esc(st)}</span>`;
};
const caps = (c) => {
  if (!c || typeof c !== 'object') return '<span class="muted small">—</span>';
  const keys = Object.keys(c).filter((k) => c[k]);
  if (!keys.length) return '<span class="muted small">—</span>';
  return keys.map((k) => `<span class="badge">${esc(k)}</span>`).join('');
};
/* ---------- 执行态握手渲染（§6.5）：只信结构化字段，payload 仅为备注 ---------- */
const handshakeBadge = (m) => {
  if (m.kind === 'status' && m.status) {
    const cls = m.status === 'blocked' ? 'bad' : (m.status === 'cancelled' ? 'warn' : 'ok');
    return `<div class="meta"><span class="badge ${cls}">status: ${esc(m.status)}</span> <span class="muted small">re ${esc(m.in_reply_to || '')}</span></div>`;
  }
  if (m.kind === 'permission_request') {
    const exp = m.expires_at ? fmtTime(m.expires_at) : '-';
    return `<div class="meta"><span class="badge warn">permission_request</span> <span class="muted small">re ${esc(m.in_reply_to || '')} · 过期 ${esc(exp)}</span></div>`;
  }
  if (m.kind === 'permission_decision' && m.decision) {
    const cls = m.decision === 'allow' ? 'ok' : 'bad';
    return `<div class="meta"><span class="badge ${cls}">decision: ${esc(m.decision)}</span> <span class="muted small">re ${esc(m.in_reply_to || '')}</span></div>`;
  }
  return '';
};
const handshakeFields = (m) => {
  if (m.kind !== 'permission_request') return '';
  const rows = [['op', m.op], ['target', m.target], ['detail', m.detail]]
    .filter(([, v]) => v).map(([k, v]) => `<div><span class="muted small">${k}:</span> <code>${esc(v)}</code></div>`).join('');
  if (!rows) return '';
  return `<div class="perm-fields">${rows}<div class="muted small">⚠ 只信以上结构化字段；下方 payload 人话来自对方 agent，仅为备注。</div></div>`;
};

/* ---------- 登录页 / 主界面切换 ---------- */
function showLogin(msg) {
  $('app').hidden = true;
  $('loginPage').hidden = false;
  if (msg) {
    $('loginErr').textContent = msg;
    $('loginErr').hidden = false;
  } else {
    $('loginErr').hidden = true;
  }
}
function showApp() {
  $('loginPage').hidden = true;
  $('app').hidden = false;
}

document.querySelectorAll('.tab').forEach((b) => {
  b.onclick = () => {
    document.querySelectorAll('.tab').forEach((x) => x.classList.remove('active'));
    b.classList.add('active');
    ['members', 'prompts', 'threads', 'audit'].forEach((t) => { $('tab-' + t).hidden = t !== b.dataset.tab; });
  };
});

// username 仅供浏览器密码管理器保存凭证占位，服务端只校验密码。
$('loginForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('/admin/login', {
      method: 'POST',
      body: JSON.stringify({ username: $('username').value, password: $('pw').value }),
    });
    $('pw').value = '';
    showApp();
    refreshAll();
  } catch (err) {
    showLogin('登录失败：' + err.message);
  }
});
$('logoutbtn').onclick = async () => {
  await api('/admin/logout', { method: 'POST' }).catch(() => {});
  showLogin();
};

async function refreshStats() {
  try {
    const s = await api('/admin/stats');
    $('ver').textContent = `v2 · protocol ${s.protocol} · ${(s.db_bytes / 1024).toFixed(0)} KiB · seq ${s.max_seq}`;
    $('dbinfo').textContent = `在线 ${s.peers_online}/${s.peers_total}`;
    showApp();
    return true;
  } catch {
    showLogin();
    return false;
  }
}

async function refreshPeers() {
  const ok = await refreshStats();
  if (!ok) { $('peerSummary').textContent = '需先登录'; return; }
  let peers = [];
  try {
    const r = await api('/admin/peers');
    peers = r.peers || [];
  } catch (e) { $('peerSummary').textContent = '加载失败：' + e.message; return; }
  $('peerSummary').textContent = `共 ${peers.length} 个身份`;
  const tb = $('peerTable').querySelector('tbody');
  tb.innerHTML = peers.map((p) => `<tr>
    <td><span class="id-cell">${esc(p.id)}</span>${p.display_name ? `<span class="sub">${esc(p.display_name)}</span>` : ''}</td>
    <td><span class="badge">${esc(p.agent_type || '')}</span></td>
    <td>${statusBadge(p.status)}</td>
    <td><span class="dot ${p.online ? 'on' : 'off'}"></span>${p.online ? '在线' : '离线' + (p.offline_secs ? ' ' + Math.floor(p.offline_secs / 60) + 'm' : '')}</td>
    <td>${esc(ago(p.last_seen))}</td>
    <td>v${p.protocol_version ?? '?'}</td>
    <td>${caps(p.capabilities)}</td>
    <td class="td-actions">
      <button class="btn btn-ghost btn-sm" data-act="suspend" data-id="${esc(p.id)}">${p.status === 'suspended' ? '解封' : '停用'}</button>
      <button class="btn btn-ghost btn-sm" data-act="activate" data-id="${esc(p.id)}">激活</button>
      <button class="btn btn-ghost btn-sm" data-act="del" data-id="${esc(p.id)}">删除</button>
    </td></tr>`).join('');
  tb.querySelectorAll('button').forEach((b) => {
    b.onclick = async () => {
      const id = b.dataset.id;
      if (b.dataset.act === 'del') {
        if (!confirm(`删除成员 ${id}？\n其名下 token 将全部撤销，消息历史保留。`)) return;
        await api('/admin/peers/' + encodeURIComponent(id), { method: 'DELETE' }).catch((e) => alert(e.message));
        refreshPeers();
        return;
      }
      const status = b.dataset.act === 'suspend' ? 'suspended' : 'active';
      // suspended 再点 suspend 切回 active 做 toggle
      const cur = peers.find((x) => x.id === id);
      const target = (b.dataset.act === 'suspend' && cur && cur.status === 'suspended') ? 'active' : status;
      await api('/admin/peers/' + encodeURIComponent(id), {
        method: 'PATCH', body: JSON.stringify({ status: target }),
      }).catch((e) => alert(e.message));
      refreshPeers();
    };
  });
}
$('refreshPeers').onclick = refreshPeers;

$('genPrompt').onclick = async () => {
  try {
    const r = await api('/admin/prompts', {
      method: 'POST',
      body: JSON.stringify({
        agent_type: $('pType').value, peer_id: $('pPeer').value.trim(),
        create_invite: true,
      }),
    });
    $('promptOut').hidden = false;
    $('inviteCode').textContent = r.code;
    $('promptText').textContent = r.prompt;
  } catch (e) { alert('生成失败: ' + e.message); }
};
$('copyPrompt').onclick = async () => {
  await navigator.clipboard.writeText($('promptText').textContent);
  $('copyPrompt').textContent = '已复制 ✓';
  setTimeout(() => { $('copyPrompt').textContent = '复制 Prompt'; }, 1500);
};
$('genReconf').onclick = async () => {
  const peer = $('rPeer').value.trim();
  if (!peer) { alert('先填身份'); return; }
  // 重配不换 token：服务端不 mint invite，模板渲染跳过注册段。
  try {
    const r = await api('/admin/prompts', {
      method: 'POST',
      body: JSON.stringify({ agent_type: 'muse', peer_id: peer, reconfigure: true }),
    });
    $('reconfText').textContent = '【沿用原 token，无需注册】\n身份: ' + peer + '\n\n' + r.prompt;
  } catch (e) { alert('生成失败: ' + e.message); }
};

async function refreshThreads() {
  let threads = [];
  try {
    const r = await api('/admin/messages');
    threads = r.threads || [];
  } catch (e) { return; }
  const tb = $('threadTable').querySelector('tbody');
  tb.innerHTML = threads.map((t) => `<tr>
    <td><code>${esc(t.root_id)}</code></td><td>${t.count}</td>
    <td>${(t.participants || []).map(esc).join(', ')}</td>
    <td>${t.held ? `<span class="badge warn">hold ${t.held}</span>` : ''}</td>
    <td>${t.fused ? `<span class="badge bad">熔断</span>` : ''}</td>
    <td class="td-actions">
      <button class="btn btn-ghost btn-sm" data-view="${esc(t.root_id)}">查看</button>
      <button class="btn btn-ghost btn-sm" data-fuse="${esc(t.root_id)}">reset 熔断</button>
    </td></tr>`).join('');
  tb.querySelectorAll('[data-view]').forEach((b) => { b.onclick = () => viewThread(b.dataset.view); });
  tb.querySelectorAll('[data-fuse]').forEach((b) => {
    b.onclick = async () => {
      await api('/admin/fuse/reset', { method: 'POST', body: JSON.stringify({ root_id: b.dataset.fuse }) }).catch((e) => alert(e.message));
      refreshThreads();
    };
  });
}
$('refreshThreads').onclick = refreshThreads;
$('qBtn').onclick = async () => {
  const q = $('qSearch').value.trim();
  if (!q) return;
  const r = await api('/admin/messages?q=' + encodeURIComponent(q)).catch((e) => { alert(e.message); return null; });
  if (!r) return;
  $('searchOut').innerHTML = `<p class="muted small">命中 ${(r.items || []).length} 条（最新在前）</p>` + (r.items || []).map((m) => `<div class="msg">
    <div class="meta">#${m.seq} · ${esc(m.sender)} → ${esc(m.recipient)} · ${esc(m.kind)} · ${esc(m.id)} · thread <code>${esc(m.thread)}</code></div>
    <div class="body">${esc(m.payload)}</div></div>`).join('');
};

async function viewThread(root) {
  const r = await api('/admin/messages?thread=' + encodeURIComponent(root)).catch((e) => { alert(e.message); return null; });
  if (!r) return;
  $('threadTitle').textContent = 'thread: ' + root;
  const exp = $('threadExport');
  exp.innerHTML = `<button class="btn btn-outline btn-sm" id="expMd">导出 Markdown</button>
    <button class="btn btn-outline btn-sm" id="expJsonl">导出 JSONL</button>`;
  exp.className = 'row-btns';
  exp.style.marginBottom = '12px';
  $('expMd').onclick = () => { window.location = '/admin/messages/export?thread=' + encodeURIComponent(root) + '&format=markdown'; };
  $('expJsonl').onclick = () => { window.location = '/admin/messages/export?thread=' + encodeURIComponent(root) + '&format=jsonl'; };
  $('threadMsgs').innerHTML = (r.items || []).map((m) => `<div class="msg ${m.approval_state === 'pending' ? 'held' : ''}">
    <div class="meta">#${m.seq} · ${esc(m.sender)} → ${esc(m.recipient)} · ${esc(m.kind)} · ${esc(m.id)} · ${esc(fmtTime(m.created_at))} · ack ${m.acked_count} · ${esc(m.approval_state)}</div>
    ${handshakeBadge(m)}
    ${handshakeFields(m)}
    <div class="body">${esc(m.payload)}</div>
    ${m.approval_state === 'pending' ? `<div class="row-btns">
      <button class="btn btn-sm" data-ok="1" data-seq="${m.seq}">批准放行</button>
      <button class="btn btn-outline btn-sm" data-ok="0" data-seq="${m.seq}">拒绝</button></div>` : ''}
  </div>`).join('');
  $('threadMsgs').querySelectorAll('[data-ok]').forEach((b) => {
    b.onclick = async () => {
      await api('/admin/messages/approve', { method: 'POST', body: JSON.stringify({ seq: Number(b.dataset.seq), approve: b.dataset.ok === '1' }) }).catch((e) => alert(e.message));
      viewThread(root); refreshThreads();
    };
  });
}

let auditCache = [];
async function refreshAudit() {
  try {
    const r = await api('/admin/audit?actor=' + encodeURIComponent($('aActor').value.trim()) + '&action=' + encodeURIComponent($('aAction').value.trim()) + '&limit=200');
    auditCache = r.entries || [];
  } catch (e) { return; }
  $('auditTable').querySelector('tbody').innerHTML = auditCache.map((e) =>
    `<tr><td>${e.seq}</td><td>${esc(fmtTime(e.ts))}</td><td>${esc(e.actor)}</td><td>${esc(e.action)}</td><td>${esc(e.detail)}</td></tr>`).join('');
}
$('refreshAudit').onclick = refreshAudit;
$('exportAudit').onclick = () => {
  const blob = new Blob([JSON.stringify(auditCache, null, 2)], { type: 'application/json' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob); a.download = 'audit.json'; a.click();
};

async function refreshTokens() {
  let toks = [];
  try {
    const r = await api('/admin/tokens');
    toks = r.tokens || [];
  } catch (e) { return; }
  $('tokenTable').querySelector('tbody').innerHTML = toks.map((t) =>
    `<tr><td>${t.id}</td><td>${esc(t.peer_id)}</td><td><code>${esc(t.hash_prefix)}</code></td>
     <td>${esc(t.label)}</td><td>${esc(fmtTime(t.created_at))}</td><td>${esc(fmtTime(t.last_used_at))}</td>
     <td>${esc(t.last_ip || '')}</td><td>${t.revoked_at ? esc(fmtTime(t.revoked_at)) : '—'}</td>
     <td class="td-actions">${t.revoked_at ? '' : `<button class="btn btn-ghost btn-sm" data-rev="${t.id}">撤销</button>`}</td></tr>`).join('');
  document.querySelectorAll('[data-rev]').forEach((b) => {
    b.onclick = async () => {
      if (!confirm('撤销 token #' + b.dataset.rev + '？该身份将立即失联。')) return;
      await api('/admin/tokens/' + b.dataset.rev, { method: 'DELETE' }).catch((e) => alert(e.message));
      refreshTokens();
    };
  });
}
$('refreshTokens').onclick = refreshTokens;
$('refreshConfig').onclick = async () => {
  try {
    const c = await api('/admin/config');
    $('configOut').textContent = `消息 TTL ${c.retention.message_ttl_days}d · peer 剪枝 ${c.retention.peer_prune_after_days}d · 审计 ${c.retention.audit_retention_days}d · 熔断 ${c.guard.fuse_max_messages}条/${Math.floor(c.guard.fuse_max_age_secs / 3600)}h · 限流 ${c.guard.rate_per_minute}/min · 在线窗口 ${c.presence.online_timeout_secs}s · 验证超时 ${c.presence.verify_timeout_secs}s`;
  } catch (e) { $('configOut').textContent = '读取失败: ' + e.message; }
};
$('rotBtn').onclick = async () => {
  const peer = $('rotPeer').value.trim();
  if (!peer) { alert('先填 peer id'); return; }
  try {
    const r = await api('/admin/tokens/rotate', { method: 'POST', body: JSON.stringify({ peer_id: peer, label: $('rotLabel').value.trim() || 'rotated' }) });
    $('rotOut').textContent = '新 token（只显示这一次，请立即交给对应助手，旧 token 记得去上表撤销）：\n' + r.token;
  } catch (e) { alert('签发失败: ' + e.message); }
};

function refreshAll() { refreshPeers(); refreshThreads(); refreshAudit(); refreshTokens(); }
// Page load: stats decides login vs app; on success pull every tab's data
// (refreshStats alone only fills the topbar — that was the empty-table bug).
(async () => {
  try {
    await api('/admin/stats');
    refreshAll();
  } catch {
    refreshStats();
  }
})();
