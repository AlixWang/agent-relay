// MCP gateway console tab (relay MCP v5).
import { api } from './api.js';
import { $, esc, icon, fmtTime, ago, renderPager, toast, confirmAction, setupDialog } from './utils.js';

const state = {
  view: 'services',
  page: 1,
  pageSize: 20,
  refreshSeq: 0,
  health: null,
  services: [],
  credTarget: null,
  permTarget: null,
  audit: { actor: '', service: '', tool: '', outcome: '' },
};

function auditQuery() {
  const params = new URLSearchParams({ page: state.page, page_size: state.pageSize });
  for (const [k, v] of Object.entries(state.audit)) {
    if (v) params.set(k === 'service' ? 'service_id' : k, v);
  }
  return params;
}

function setHeaders(table, labels) {
  const thead = table.querySelector('thead');
  thead.innerHTML = `<tr>${labels.map((l) => `<th>${esc(l)}</th>`).join('')}</tr>`;
}

function skeleton(table, cols) {
  const tbody = table.querySelector('tbody');
  tbody.innerHTML = `<tr class="skel"><td colspan="${cols}"><i></i></td></tr>`;
}

let remTimer = null;

function startRemTimers() {
  if (remTimer) clearInterval(remTimer);
  remTimer = setInterval(() => {
    let hitZero = false;
    document.querySelectorAll('[data-mcp-rem]').forEach((el) => {
      let left = Number(el.dataset.mcpRem) || 0;
      if (left > 0) {
        left -= 1;
        if (left === 0) hitZero = true;
      }
      el.dataset.mcpRem = String(left);
      el.textContent = `${left}s`;
    });
    if (hitZero) {
      stopMCP();
      refreshMCP();
    }
  }, 1000);
}

function badge(text, kind = '') {
  return `<span class="badge ${kind}">${esc(text)}</span>`;
}

function shortTime(ts) {
  return ts ? `${fmtTime(ts)} <small class="muted">${esc(ago(ts))}</small>` : '<span class="muted">—</span>';
}

function secretHintFor(service) {
  const name = service.credential_name || '';
  if (name === 'GA_ADSENSE_ADC' || name.endsWith('_ADC')) {
    return `粘贴 Google OAuth2 authorized_user ADC JSON（包含 type、client_id、client_secret、refresh_token）。AdSense 与 GA 共用此密钥（${name}）。`;
  }
  if (name === 'BING_WM_API_KEY') {
    return '粘贴 Bing Webmaster Tools API Key 原文；保存后立即加密入库。';
  }
  return `粘贴 ${service.credential_ref || name} 对应的密钥原文；保存后以 AES-GCM 加密保存在资源中心。`;
}

async function refreshHealth() {
  try {
    const h = await api('/admin/mcp/health');
    state.health = h;
    if ($('mcpEnabled')) $('mcpEnabled').textContent = h.enabled ? '开启' : '关闭';
    if ($('mcpWriteEnabled')) $('mcpWriteEnabled').textContent = h.write_enabled ? '开启' : '关闭';
    if ($('mcpPending')) $('mcpPending').textContent = h.pending_approvals ?? 0;
    if ($('mcpKeys')) $('mcpKeys').textContent = h.keys ?? 0;
    if ($('mcpServices')) $('mcpServices').textContent = (h.services || []).length;
    if ($('mcpToggleEnabled')) $('mcpToggleEnabled').checked = !!h.enabled;
    if ($('mcpToggleWrite')) $('mcpToggleWrite').checked = !!h.write_enabled;
    if ($('mcpResetPolicy')) $('mcpResetPolicy').hidden = !h.has_runtime_overrides;
    const badgeEl = $('mcpApprovalBadge');
    if (badgeEl) {
      badgeEl.hidden = !h.pending_approvals;
      badgeEl.textContent = h.pending_approvals || '';
    }
    const nav = $('navMCP');
    if (nav) {
      nav.hidden = !h.pending_approvals;
      nav.textContent = h.pending_approvals || '';
    }
  } catch {
    // The page-level table error is the useful signal; stats stay "-".
  }
}

export async function refreshMCP() {
  const table = $('mcpTable');
  if (!table) return;
  const seq = ++state.refreshSeq;
  const seg = $('mcpSeg');
  if (seg) {
    seg.querySelectorAll('button').forEach((b) => b.classList.toggle('on', b.dataset.v === state.view));
  }
  $('mcpNewKey').hidden = state.view !== 'keys';
  $('mcpExport').hidden = state.view !== 'audit';
  $('mcpAuditFilters').hidden = state.view !== 'audit';
  if (state.view !== 'approvals') stopMCP();
  await refreshHealth();
  if (seq !== state.refreshSeq) return;
  try {
    if (state.view === 'services') await renderServices(table, seq);
    else if (state.view === 'keys') await renderKeys(table, seq);
    else if (state.view === 'peers') await renderPeers(table, seq);
    else if (state.view === 'approvals') await renderApprovals(table, seq);
    else await renderAudit(table, seq);
  } catch (err) {
    if (seq !== state.refreshSeq || err.status === 401) return;
    const cols = table.querySelectorAll('thead th').length || 1;
    table.querySelector('tbody').innerHTML = `<tr><td colspan="${cols}" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
    $('mcpNote').textContent = '加载失败：' + err.message;
  }
}

function renderWriteCell(s) {
  const writeTools = s.write_tools || [];
  if (!writeTools.length) {
    return `${badge('只读服务')}<br><small class="muted">无写工具</small>`;
  }
  const allow = s.write_allowed_tools || [];
  const head = s.write_enabled ? badge('写总开关开', 'warn') : badge('写总开关关');
  const detail = allow.length ? allow.map((t) => `<code>${esc(t)}</code>`).join(', ') : '未允许任何写工具';
  return `${head}<br><small class="muted">${detail}</small>`;
}

function renderCredCell(s) {
  if (!s.credential_required) {
    return '<span class="muted">无需凭证</span>';
  }
  const st = s.credential_configured ? badge('已配置', 'ok') : badge('未配置', 'bad');
  const ref = s.credential_name || s.credential_ref || '—';
  return `${st} <code>${esc(ref)}</code><br><small class="muted">资源中心加密密钥</small>`;
}

async function renderServices(table, seq) {
  setHeaders(table, ['服务', '状态', '工具', '写权限', '凭证（资源中心密钥）', '最近调用', '操作']);
  skeleton(table, 7);
  const r = await api('/admin/mcp/services');
  if (seq !== state.refreshSeq) return;
  const rows = r.services || [];
  state.services = rows;
  if ($('mcpResetPolicy') && r.has_runtime_overrides != null) {
    $('mcpResetPolicy').hidden = !r.has_runtime_overrides;
  }
  if (!rows.length) {
    table.querySelector('tbody').innerHTML = `<tr><td colspan="7" class="empty-td"><div class="empty">${icon('server')}<p>尚未注册 MCP 服务</p></div></td></tr>`;
    renderPager($('mcpPager'), { total: 0 });
    return;
  }
  table.querySelector('tbody').innerHTML = rows.map((s) => {
    const missingCred = s.enabled && s.credential_required && !s.credential_configured;
    const errPart = s.last_error
      ? `<br><span class="badge bad">${esc(s.last_error.code || 'error')}</span> <small class="muted">${esc(s.last_error.tool || '')}</small>`
      : '';
    return `<tr>
      <td><b>${esc(s.display_name || s.id)}</b><br><code>${esc(s.id)}</code> <span class="muted xs">${esc(s.version || '')}</span></td>
      <td>${s.enabled ? badge('启用', 'ok') : badge('禁用', 'bad')}${missingCred ? ` ${badge('缺凭证', 'warn')}` : ''}<br><small class="muted">${s.effective_enabled ? '可被调用' : s.enabled ? '等待凭证' : '已暂停'}</small></td>
      <td>${esc(s.enabled_tool_count ?? 0)} / ${esc(s.tool_count ?? 0)}</td>
      <td>${renderWriteCell(s)}</td>
      <td>${renderCredCell(s)}</td>
      <td>${shortTime(s.last_call_at)}${errPart}</td>
      <td class="td-actions">
        <div class="mcp-actions-cell">
          ${s.credential_name ? `<button class="btn btn-outline btn-xs" data-cred="${esc(s.id)}">${s.credential_configured ? '更新凭证' : '配置凭证'}</button>` : ''}
          <button class="btn btn-outline btn-xs" data-perm="${esc(s.id)}">权限配置</button>
          <button class="btn btn-ghost btn-xs ${s.enabled ? 'danger' : ''}" data-svc-toggle="${esc(s.id)}" data-next="${s.enabled ? '0' : '1'}">${s.enabled ? '禁用' : '启用'}</button>
        </div>
      </td>
    </tr>`;
  }).join('');

  table.querySelectorAll('button[data-cred]').forEach((b) => {
    b.onclick = () => {
      const svc = state.services.find((x) => x.id === b.dataset.cred);
      if (svc) openCredDialog(svc);
    };
  });
  table.querySelectorAll('button[data-perm]').forEach((b) => {
    b.onclick = () => {
      const svc = state.services.find((x) => x.id === b.dataset.perm);
      if (svc) openPermDialog(svc);
    };
  });
  table.querySelectorAll('button[data-svc-toggle]').forEach((b) => {
    b.onclick = async () => {
      const nextEnabled = b.dataset.next === '1';
      try {
        await api(`/admin/mcp/services/${encodeURIComponent(b.dataset.svcToggle)}`, {
          method: 'PUT', body: JSON.stringify({ enabled: nextEnabled }),
        });
        toast(nextEnabled ? '服务已启用' : '服务已禁用', 'ok');
        refreshMCP();
      } catch (err) { toast('操作失败：' + err.message, 'bad'); }
    };
  });

  $('mcpNote').textContent = '凭证存储在「资源中心」加密密钥库中（非环境变量），配置后即时生效；开关与工具白名单保存在数据库，修改无需重启。';
  renderPager($('mcpPager'), { total: 0 });
}

function openCredDialog(svc) {
  const dlg = $('mcpCredDlg');
  if (!dlg) return;
  state.credTarget = svc;
  $('mcpCredTitle').textContent = `${svc.credential_configured ? '更新' : '配置'} ${svc.display_name || svc.id} 凭证`;
  $('mcpCredName').textContent = svc.credential_name || svc.credential_ref || '—';
  $('mcpCredHint').textContent = secretHintFor(svc);
  const valEl = $('mcpCredValue');
  valEl.value = '';
  valEl.placeholder = (svc.credential_name || '').endsWith('_ADC')
    ? '{\n  "type": "authorized_user",\n  "client_id": "...",\n  "client_secret": "...",\n  "refresh_token": "..."\n}'
    : '粘贴密钥原文';
  $('mcpCredDesc').value = `${svc.display_name || svc.id} MCP gateway credential`;
  $('mcpCredBtn').disabled = false;
  dlg.showModal();
}

function openPermDialog(svc) {
  const dlg = $('mcpPermDlg');
  if (!dlg) return;
  state.permTarget = svc;
  $('mcpPermTitle').textContent = `权限配置 · ${svc.display_name || svc.id}`;
  $('mcpPermSub').textContent = `服务 ID: ${svc.id} · 控制服务开关与允许暴露的工具白名单`;
  $('mcpPermEnabled').checked = !!svc.enabled;
  const writeTools = svc.write_tools || [];
  const writeWarn = $('mcpPermWriteWarn');
  if (writeWarn) {
    writeWarn.hidden = !writeTools.length || !!svc.write_enabled;
  }

  const writeBox = $('mcpPermWriteList');
  if (!writeTools.length) {
    writeBox.innerHTML = '<div class="muted xs">该服务当前没有已注册写工具（只读服务）。</div>';
  } else {
    writeBox.innerHTML = writeTools.map((t) => {
      const riskLabel = t.risk === 'high' ? badge('高风险 · 需二次批准', 'bad') : badge('低风险写', 'warn');
      return `<div class="mcp-perm-row">
        <label class="check mcp-perm-head">
          <input type="checkbox" data-write-tool="${esc(t.name)}" ${t.in_allowlist ? 'checked' : ''}>
          <code>${esc(t.name)}</code> ${riskLabel}
        </label>
        ${t.description ? `<div class="mcp-perm-meta">${esc(t.description)}</div>` : ''}
      </div>`;
    }).join('');
  }

  const readTools = svc.read_tools || [];
  const allowed = svc.allowed_tools || [];
  const allowAll = allowed.includes('*');
  const readSet = new Set(allowed);
  const readAllEl = $('mcpPermReadAll');
  readAllEl.checked = allowAll;
  const readBox = $('mcpPermReadList');
  if (!readTools.length) {
    readBox.innerHTML = '<div class="muted xs">无已注册读工具。</div>';
  } else {
    readBox.innerHTML = readTools.map((t) => {
      const checked = allowAll || readSet.has(t.name);
      return `<div class="mcp-perm-row">
        <label class="check mcp-perm-head">
          <input type="checkbox" data-read-tool="${esc(t.name)}" ${checked ? 'checked' : ''} ${allowAll ? 'disabled' : ''}>
          <code>${esc(t.name)}</code>
        </label>
        ${t.description ? `<div class="mcp-perm-meta">${esc(t.description)}</div>` : ''}
      </div>`;
    }).join('');
  }
  readAllEl.onchange = () => {
    const all = readAllEl.checked;
    readBox.querySelectorAll('input[data-read-tool]').forEach((cb) => {
      cb.disabled = all;
      if (all) cb.checked = true;
    });
  };
  $('mcpPermBtn').disabled = false;
  dlg.showModal();
}

async function renderKeys(table, seq) {
  setHeaders(table, ['名称', 'Scope', '前缀', '有效期', '最后使用', '状态', '操作']);
  skeleton(table, 7);
  const r = await api('/admin/mcp/keys');
  if (seq !== state.refreshSeq) return;
  const rows = r.keys || [];
  if (!rows.length) {
    table.querySelector('tbody').innerHTML = `<tr><td colspan="7" class="empty-td"><div class="empty">${icon('key')}<p>还没有外部 MCP Key；点右上角「新建 Key」</p></div></td></tr>`;
    renderPager($('mcpPager'), { total: 0 });
    return;
  }
  table.querySelector('tbody').innerHTML = rows.map((k) => {
    const dead = k.revoked || k.expired;
    const scheduled = !dead && k.revoked_at > 0;
    return `<tr>
      <td><b>${esc(k.name)}</b><br><code>${esc(k.id)}</code><br><small class="muted">${esc(k.note || '')}</small></td>
      <td>${(k.scopes || []).map((s) => `<code>${esc(s)}</code>`).join('<br>')}</td>
      <td><code>${esc(k.key_prefix)}…</code></td>
      <td>${k.expires_at ? shortTime(k.expires_at) : '<span class="muted">不过期</span>'}</td>
      <td>${k.last_used_at ? shortTime(k.last_used_at) + `<br><small class="muted">${esc(k.last_used_ip || '')} · ${esc(k.use_count)} 次</small>` : '<span class="muted">从未</span>'}</td>
      <td>${dead ? badge(k.revoked ? '已吊销' : '已过期', 'bad') : scheduled ? badge('待失效', 'warn') : badge('有效', 'ok')}</td>
      <td class="td-actions">${dead ? '<span class="muted xs">—</span>' : `
        ${scheduled ? '' : `<button class="btn-icon" data-rotate="${esc(k.id)}" data-tip="轮换" aria-label="轮换">${icon('rotate')}</button>`}
        <button class="btn-icon danger" data-revoke="${esc(k.id)}" data-name="${esc(k.name)}" data-tip="吊销" aria-label="吊销">${icon('ban')}</button>`}
      </td>
    </tr>`;
  }).join('');
  table.querySelectorAll('button[data-rotate]').forEach((b) => {
    b.onclick = async () => {
      const ok = await confirmAction({
        title: '轮换这把 Key？',
        body: '会立即签发一把同 scope 的新 Key，旧 Key 按配置宽限后失效。新原文只显示一次。',
        danger: false, okText: '轮换',
      });
      if (!ok) return;
      try {
        const res = await api(`/admin/mcp/keys/${encodeURIComponent(b.dataset.rotate)}/rotate`, { method: 'POST' });
        showKeyResult(res, { rotated: true });
        refreshMCP();
      } catch (err) { toast('轮换失败：' + err.message, 'bad'); }
    };
  });
  table.querySelectorAll('button[data-revoke]').forEach((b) => {
    b.onclick = async () => {
      const ok = await confirmAction({
        title: `吊销 Key「${b.dataset.name}」？`,
        body: '吊销即时生效，所有使用该 Key 的客户端都会收到 401，无法恢复。',
        danger: true, okText: '确认吊销',
      });
      if (!ok) return;
      try {
        await api(`/admin/mcp/keys/${encodeURIComponent(b.dataset.revoke)}/revoke`, { method: 'POST' });
        toast('Key 已吊销', 'ok');
        refreshMCP();
      } catch (err) { toast('吊销失败：' + err.message, 'bad'); }
    };
  });
  $('mcpNote').textContent = 'Key 只存 SHA-256；原文只在签发/轮换时显示一次。新建 Key 默认只勾读，写要单独加 mcp:<service>:write:<tool>。';
  renderPager($('mcpPager'), { total: 0 });
}

async function renderPeers(table, seq) {
  setHeaders(table, ['Peer', '成员状态', 'MCP 授权', 'Scope', '备注', '操作']);
  skeleton(table, 6);
  const r = await api('/admin/mcp/peers');
  if (seq !== state.refreshSeq) return;
  const rows = r.peers || [];
  if (!rows.length) {
    table.querySelector('tbody').innerHTML = `<tr><td colspan="6" class="empty-td"><div class="empty">${icon('users')}<p>还没有注册助手</p></div></td></tr>`;
    renderPager($('mcpPager'), { total: 0 });
    return;
  }
  table.querySelector('tbody').innerHTML = rows.map((p) => `<tr>
    <td><b>${esc(p.peer_id)}</b><br><small class="muted">${esc(p.agent_type || '')}</small></td>
    <td>${badge(p.status || 'unknown', p.status === 'active' ? 'ok' : p.status === 'suspended' ? 'bad' : '')}</td>
    <td>${p.authorized ? badge('已授权', 'ok') : badge('未授权', 'bad')}<br><small class="muted">${esc(p.mcp_status || 'none')}</small></td>
    <td>${(p.scopes || []).map((s) => `<code>${esc(s)}</code>`).join('<br>') || '<span class="muted">—</span>'}</td>
    <td>${esc(p.note || '')}</td>
    <td class="td-actions">
      <button class="btn btn-ghost btn-xs" data-grant="${esc(p.peer_id)}" data-scopes="${esc((p.scopes || []).join(','))}" data-note="${esc(p.note || '')}">${p.authorized ? '编辑' : '授权'}</button>
      ${p.authorized ? `<button class="btn btn-ghost btn-xs danger" data-revoke="${esc(p.peer_id)}">撤销</button>` : ''}
    </td>
  </tr>`).join('');
  table.querySelectorAll('button[data-grant]').forEach((b) => {
    b.onclick = async () => {
      const scopes = window.prompt(
        '填写 scope，逗号分隔（例如 mcp:adsense:read:*,mcp:bing-webmaster:read:*,mcp:ga:read:*）',
        b.dataset.scopes || 'mcp:adsense:read:*,mcp:bing-webmaster:read:*,mcp:ga:read:*',
      );
      if (scopes === null) return;
      const list = scopes.split(/[\n,]+/).map((s) => s.trim()).filter(Boolean);
      if (!list.length) { toast('至少一个 scope', 'warn'); return; }
      try {
        await api(`/admin/mcp/peers/${encodeURIComponent(b.dataset.grant)}`, {
          method: 'PUT', body: JSON.stringify({ scopes: list, enabled: true, note: b.dataset.note || '' }),
        });
        toast('Peer 授权已保存', 'ok');
        refreshMCP();
      } catch (err) { toast('授权失败：' + err.message, 'bad'); }
    };
  });
  table.querySelectorAll('button[data-revoke]').forEach((b) => {
    b.onclick = async () => {
      const ok = await confirmAction({
        title: '撤销该 Peer 的 MCP 授权？',
        body: '下一次调用即按无授权处理（403）；可随时重新授权。',
        danger: true, okText: '撤销',
      });
      if (!ok) return;
      try {
        await api(`/admin/mcp/peers/${encodeURIComponent(b.dataset.revoke)}`, { method: 'DELETE' });
        toast('Peer 授权已撤销', 'ok');
        refreshMCP();
      } catch (err) { toast('撤销失败：' + err.message, 'bad'); }
    };
  });
  $('mcpNote').textContent = 'relay 助手用 relay token 调 /mcp；授权物化在 mcp_keys 的 peer 行，scope 变更即时生效。';
  renderPager($('mcpPager'), { total: 0 });
}

async function renderApprovals(table, seq) {
  setHeaders(table, ['发起方', '工具', '状态', '冻结参数', '请求时间', '剩余', '操作']);
  skeleton(table, 7);
  const r = await api(`/admin/mcp/approvals?page=${state.page}&page_size=${state.pageSize}`);
  if (seq !== state.refreshSeq) return;
  const rows = r.approvals || [];
  if (!rows.length) {
    stopMCP();
    table.querySelector('tbody').innerHTML = `<tr><td colspan="7" class="empty-td"><div class="empty">${icon('shield')}<p>没有待批准或历史审批记录</p></div></td></tr>`;
    renderPager($('mcpPager'), { total: 0 });
    return;
  }
  table.querySelector('tbody').innerHTML = rows.map((a) => `<tr>
    <td><b>${esc(a.principal)}</b><br><small class="muted">${esc(a.auth_kind || '')} · ${esc(a.key_name || '')}</small></td>
    <td><b>${esc(a.tool)}</b><br><small class="muted">${esc(a.service_id)}</small><br><small class="muted">${esc(a.id)}</small></td>
    <td>${badge(a.status, a.status === 'pending' ? 'warn' : a.status === 'approved' || a.status === 'executed' ? 'ok' : 'bad')}<br>${a.outcome ? `<small class="muted">${esc(a.outcome)}${a.error_code ? ' · ' + esc(a.error_code) : ''}</small>` : ''}</td>
    <td><pre class="mcp-params">${esc(JSON.stringify(a.params ?? a.params_json ?? {}, null, 2))}</pre><small class="muted">${esc((a.params_hash || '').slice(0, 16))}…</small></td>
    <td>${shortTime(a.requested_at)}</td>
    <td>${a.status === 'pending' || a.status === 'approved' ? `<span data-mcp-rem="${esc(a.remaining_seconds || 0)}">${esc(a.remaining_seconds || 0)}s</span>` : '<span class="muted">—</span>'}</td>
    <td class="td-actions">${a.status === 'pending' ? `
      <button class="btn btn-ghost btn-xs" data-approve="${esc(a.id)}">批准</button>
      <button class="btn btn-ghost btn-xs danger" data-reject="${esc(a.id)}">拒绝</button>` : '<span class="muted xs">—</span>'}
    </td>
  </tr>`).join('');
  table.querySelectorAll('button[data-approve]').forEach((b) => {
    b.onclick = async () => {
      const ok = await confirmAction({
        title: '批准这次高风险写？',
        body: '批准后执行窗口 10 分钟，参数已冻结；发起方必须在窗口内带 approval_id 再调一次，且只能执行一次。',
        danger: false, okText: '批准',
      });
      if (!ok) return;
      try {
        await api(`/admin/mcp/approvals/${encodeURIComponent(b.dataset.approve)}/approve`, { method: 'POST' });
        toast('已批准', 'ok');
        refreshMCP();
      } catch (err) { toast('批准失败：' + err.message, 'bad'); }
    };
  });
  table.querySelectorAll('button[data-reject]').forEach((b) => {
    b.onclick = async () => {
      const note = window.prompt('拒绝原因（可选）', '');
      if (note === null) return;
      try {
        await api(`/admin/mcp/approvals/${encodeURIComponent(b.dataset.reject)}/reject`, { method: 'POST', body: JSON.stringify({ note }) });
        toast('已拒绝', 'ok');
        refreshMCP();
      } catch (err) { toast('拒绝失败：' + err.message, 'bad'); }
    };
  });
  $('mcpNote').textContent = '待批截止 24 小时；批准后执行窗口 10 分钟；一个 approval_id 只执行一次，重放会返回 approval_already_used。';
  startRemTimers();
  renderPager($('mcpPager'), {
    total: r.total || 0, page: state.page, pageSize: state.pageSize,
    onPage: (p) => { state.page = p; refreshMCP(); },
    onPageSize: (s) => { state.pageSize = s; state.page = 1; refreshMCP(); },
  });
}

async function renderAudit(table, seq) {
  setHeaders(table, ['时间', 'Actor', '服务 / 工具', '结果', '错误', '耗时', 'IP', '参数摘要']);
  skeleton(table, 8);
  const params = auditQuery();
  const exp = $('mcpExport');
  if (exp) exp.onclick = () => { window.location.href = '/admin/mcp/audit/export?' + params.toString(); };
  const r = await api('/admin/mcp/audit?' + params.toString());
  if (seq !== state.refreshSeq) return;
  const rows = r.entries || [];
  if (!rows.length) {
    table.querySelector('tbody').innerHTML = `<tr><td colspan="8" class="empty-td"><div class="empty">${icon('scroll')}<p>暂无 MCP 审计记录</p></div></td></tr>`;
    renderPager($('mcpPager'), { total: 0 });
    return;
  }
  table.querySelector('tbody').innerHTML = rows.map((a) => `<tr>
    <td>${shortTime(a.ts)}</td>
    <td><b>${esc(a.actor)}</b><br><small class="muted">${esc(a.auth_kind || '')} ${esc(a.key_name || '')}</small></td>
    <td>${esc(a.service_id || '—')}<br><code>${esc(a.tool || '—')}</code></td>
    <td>${badge(a.outcome || '?', a.outcome === 'ok' || a.outcome === 'executed' ? 'ok' : a.outcome === 'denied' || a.outcome === 'error' ? 'bad' : 'warn')}<br><small class="muted">HTTP ${esc(a.http_status || 0)} · 上游 ${esc(a.upstream_status || 0)}</small></td>
    <td>${a.error_code ? `<code>${esc(a.error_code)}</code>` : '<span class="muted">—</span>'}</td>
    <td>${esc(a.latency_ms || 0)} ms</td>
    <td><code>${esc(a.client_ip || '')}</code></td>
    <td><small>${esc(a.params_summary || '')}</small>${a.detail ? `<br><small class="muted">${esc(a.detail)}</small>` : ''}</td>
  </tr>`).join('');
  $('mcpNote').textContent = '审计写 180 天 / 读 90 天，行数上限 200 万；每条同时镜像到现有 audit_log。导出最多 1 万行。';
  renderPager($('mcpPager'), {
    total: r.total || 0, page: state.page, pageSize: state.pageSize,
    onPage: (p) => { state.page = p; refreshMCP(); },
    onPageSize: (s) => { state.pageSize = s; state.page = 1; refreshMCP(); },
  });
}

function showKeyResult(res, { rotated = false } = {}) {
  const k = res.key || {};
  const dlg = $('mcpKeyDlg');
  if (!dlg) return;
  const title = $('mcpKeyTitle');
  if (title) title.textContent = rotated ? '轮换后的新 Key' : 'Key 已签发';
  const fields = $('mcpKeyFields');
  if (fields) fields.hidden = true;
  const btn = $('mcpKeyBtn');
  if (btn) { btn.hidden = true; btn.disabled = true; }
  $('mcpKeyPlain').textContent = res.plaintext || '';
  $('mcpKeyResult').hidden = false;
  $('mcpKeyMeta').textContent = `${k.name || ''} · ${(k.scopes || []).join(', ')} · 到期 ${k.expires_at ? fmtTime(k.expires_at) : '不过期'}`;
  if (!dlg.open) dlg.showModal();
}

export function stopMCP() {
  if (remTimer) {
    clearInterval(remTimer);
    remTimer = null;
  }
}

export function initMCP() {
  const seg = $('mcpSeg');
  if (seg) {
    seg.querySelectorAll('button').forEach((b) => {
      b.onclick = () => {
        state.view = b.dataset.v;
        state.page = 1;
        refreshMCP();
      };
    });
  }
  const refresh = $('mcpRefresh');
  if (refresh) refresh.onclick = refreshMCP;

  const toggleEnabled = $('mcpToggleEnabled');
  if (toggleEnabled) {
    toggleEnabled.onchange = async () => {
      const next = toggleEnabled.checked;
      if (!next) {
        const ok = await confirmAction({
          title: '关闭 MCP 网关总开关？',
          body: '关闭后 POST /mcp* 会立即返回 503，直到重新开启。',
          danger: true, okText: '确认关闭',
        });
        if (!ok) {
          toggleEnabled.checked = true;
          return;
        }
      }
      toggleEnabled.disabled = true;
      try {
        await api('/admin/mcp/config', { method: 'PUT', body: JSON.stringify({ enabled: next }) });
        toast(next ? 'MCP 网关已开启' : 'MCP 网关已关闭', 'ok');
        refreshMCP();
      } catch (err) {
        toggleEnabled.checked = !next;
        toast('切换失败：' + err.message, 'bad');
      } finally {
        toggleEnabled.disabled = false;
      }
    };
  }

  const toggleWrite = $('mcpToggleWrite');
  if (toggleWrite) {
    toggleWrite.onchange = async () => {
      const next = toggleWrite.checked;
      if (next) {
        const ok = await confirmAction({
          title: '开启全局写总开关？',
          body: '开启后，各服务白名单内的写工具（如 Bing URL/Feed 提交）将被允许调用；高风险写仍需人工二次批准。',
          danger: false, okText: '确认开启',
        });
        if (!ok) {
          toggleWrite.checked = false;
          return;
        }
      }
      toggleWrite.disabled = true;
      try {
        await api('/admin/mcp/config', { method: 'PUT', body: JSON.stringify({ write_enabled: next }) });
        toast(next ? '写总开关已开启' : '写总开关已关闭', 'ok');
        refreshMCP();
      } catch (err) {
        toggleWrite.checked = !next;
        toast('切换失败：' + err.message, 'bad');
      } finally {
        toggleWrite.disabled = false;
      }
    };
  }

  const resetBtn = $('mcpResetPolicy');
  if (resetBtn) {
    resetBtn.onclick = async () => {
      const ok = await confirmAction({
        title: '恢复为 config.toml 默认策略？',
        body: '会清除控制台保存的网关开关、服务开关和工具白名单覆盖，恢复为配置文件默认值。已保存的凭证密钥不受影响。',
        danger: true, okText: '恢复默认',
      });
      if (!ok) return;
      try {
        await api('/admin/mcp/config', { method: 'DELETE' });
        toast('已恢复默认策略', 'ok');
        refreshMCP();
      } catch (err) { toast('恢复失败：' + err.message, 'bad'); }
    };
  }

  const credDlg = $('mcpCredDlg');
  if (credDlg) {
    setupDialog(credDlg);
    credDlg.addEventListener('close', () => {
      if ($('mcpCredValue')) $('mcpCredValue').value = '';
      state.credTarget = null;
    });
    const goRes = $('mcpCredGoRes');
    if (goRes) {
      goRes.onclick = () => {
        if ($('mcpCredValue')) $('mcpCredValue').value = '';
        credDlg.close();
        location.hash = '#resources';
      };
    }
    const credForm = $('mcpCredForm');
    if (credForm) {
      credForm.onsubmit = async (e) => {
        e.preventDefault();
        const svc = state.credTarget;
        const name = svc?.credential_name;
        if (!name) { toast('该服务未定义密钥名称', 'bad'); return; }
        const value = ($('mcpCredValue').value || '').trim();
        if (!value) { toast('请填写密钥值', 'warn'); return; }
        if (name.endsWith('_ADC')) {
          try {
            const parsed = JSON.parse(value);
            if (!parsed || parsed.type !== 'authorized_user' || !parsed.client_id || !parsed.client_secret || !parsed.refresh_token) {
              toast('Google ADC JSON 需包含 type=authorized_user、client_id、client_secret、refresh_token', 'warn');
              return;
            }
          } catch {
            toast('Google ADC 凭证必须是合法 JSON 对象', 'warn');
            return;
          }
        }
        const btn = $('mcpCredBtn');
        btn.disabled = true;
        try {
          await api(`/admin/resources/secret/${encodeURIComponent(name)}`, {
            method: 'PUT',
            body: JSON.stringify({
              description: ($('mcpCredDesc').value || '').trim(),
              value,
            }),
          });
          $('mcpCredValue').value = '';
          credDlg.close();
          toast(`密钥 ${name} 已保存并生效`, 'ok');
          refreshMCP();
        } catch (err) {
          toast('保存凭证失败：' + err.message, 'bad');
          btn.disabled = false;
        }
      };
    }
  }

  const permDlg = $('mcpPermDlg');
  if (permDlg) {
    setupDialog(permDlg);
    permDlg.addEventListener('close', () => { state.permTarget = null; });
    const permForm = $('mcpPermForm');
    if (permForm) {
      permForm.onsubmit = async (e) => {
        e.preventDefault();
        const svc = state.permTarget;
        if (!svc) return;
        const enabled = $('mcpPermEnabled').checked;
        let allowedTools = ['*'];
        if (!$('mcpPermReadAll').checked) {
          allowedTools = [...$('mcpPermReadList').querySelectorAll('input[data-read-tool]:checked')].map((cb) => cb.dataset.readTool);
          if (!allowedTools.length) {
            toast('读工具白名单至少保留 1 个工具（如需完全停用请关闭服务开关）', 'warn');
            return;
          }
        }
        const writeAllowed = [...$('mcpPermWriteList').querySelectorAll('input[data-write-tool]:checked')].map((cb) => cb.dataset.writeTool);
        const btn = $('mcpPermBtn');
        btn.disabled = true;
        try {
          await api(`/admin/mcp/services/${encodeURIComponent(svc.id)}`, {
            method: 'PUT',
            body: JSON.stringify({
              enabled,
              allowed_tools: allowedTools,
              write_allowed_tools: writeAllowed,
            }),
          });
          permDlg.close();
          toast('服务权限策略已保存', 'ok');
          refreshMCP();
        } catch (err) {
          toast('保存失败：' + err.message, 'bad');
          btn.disabled = false;
        }
      };
    }
  }

  const dryDlg = $('mcpDryDlg');
  if (dryDlg) {
    setupDialog(dryDlg);
    const openDry = $('mcpDryRun');
    if (openDry) {
      openDry.onclick = () => {
        $('mcpDryService').value = 'bing-webmaster';
        $('mcpDryTool').value = 'submit_url';
        $('mcpDryParams').value = '{\n  "site": "https://example.com/",\n  "url": "https://example.com/test"\n}';
        $('mcpDryResult').hidden = true;
        $('mcpDryBtn').disabled = false;
        dryDlg.showModal();
      };
    }
    const dryForm = $('mcpDryForm');
    if (dryForm) {
      dryForm.onsubmit = async (e) => {
        e.preventDefault();
        let params = {};
        try { params = JSON.parse($('mcpDryParams').value || '{}'); }
        catch { toast('参数不是合法 JSON', 'bad'); return; }
        const btn = $('mcpDryBtn');
        btn.disabled = true;
        try {
          const out = await api('/admin/mcp/dry-run', {
            method: 'POST',
            body: JSON.stringify({
              service: $('mcpDryService').value.trim(),
              tool: $('mcpDryTool').value.trim(),
              params,
            }),
          });
          const pre = $('mcpDryResult');
          pre.hidden = false;
          pre.textContent = JSON.stringify(out, null, 2);
          toast(out.valid ? '参数校验通过' : '参数或工具未通过校验', out.valid ? 'ok' : 'warn');
        } catch (err) {
          toast('Dry-run 失败：' + err.message, 'bad');
        } finally {
          btn.disabled = false;
        }
      };
    }
  }

  const applyAudit = $('mcpAuditApply');
  if (applyAudit) {
    applyAudit.onclick = () => {
      state.audit = {
        actor: $('mcpAuditActor').value.trim(),
        service: $('mcpAuditService').value.trim(),
        tool: $('mcpAuditTool').value.trim(),
        outcome: $('mcpAuditOutcome').value,
      };
      state.page = 1;
      refreshMCP();
    };
  }
  const clearAudit = $('mcpAuditClear');
  if (clearAudit) {
    clearAudit.onclick = () => {
      $('mcpAuditActor').value = '';
      $('mcpAuditService').value = '';
      $('mcpAuditTool').value = '';
      $('mcpAuditOutcome').value = '';
      state.audit = { actor: '', service: '', tool: '', outcome: '' };
      state.page = 1;
      refreshMCP();
    };
  }

  const dlg = $('mcpKeyDlg');
  if (dlg) {
    setupDialog(dlg);
    dlg.addEventListener('close', () => {
      const plain = $('mcpKeyPlain');
      if (plain) plain.textContent = '';
      const result = $('mcpKeyResult');
      if (result) result.hidden = true;
    });
    const open = $('mcpNewKey');
    if (open) {
      open.onclick = () => {
        const title = $('mcpKeyTitle');
        if (title) title.textContent = '新建 MCP Key';
        const fields = $('mcpKeyFields');
        if (fields) fields.hidden = false;
        $('mcpKeyName').value = '';
        $('mcpKeyScopes').value = 'mcp:adsense:read:*\nmcp:bing-webmaster:read:*\nmcp:ga:read:*';
        $('mcpKeyDays').value = '0';
        $('mcpKeyNote').value = '';
        $('mcpKeyResult').hidden = true;
        const btn = $('mcpKeyBtn');
        if (btn) { btn.hidden = false; btn.disabled = false; }
        dlg.showModal();
      };
    }
    const form = $('mcpKeyForm');
    if (form) {
      form.onsubmit = async (e) => {
        e.preventDefault();
        const scopes = $('mcpKeyScopes').value.split(/[\n,]+/).map((s) => s.trim()).filter(Boolean);
        if (!scopes.length) { toast('至少填一个 scope', 'warn'); return; }
        const btn = $('mcpKeyBtn');
        btn.disabled = true;
        try {
          const r = await api('/admin/mcp/keys', {
            method: 'POST',
            body: JSON.stringify({
              name: $('mcpKeyName').value.trim(),
              scopes,
              expires_in_days: Number($('mcpKeyDays').value) || 0,
              note: $('mcpKeyNote').value.trim(),
            }),
          });
          showKeyResult(r);
          toast('Key 已签发', 'ok');
          refreshMCP();
        } catch (err) {
          toast('签发失败：' + err.message, 'bad');
          btn.disabled = false;
        }
      };
    }
  }
}
