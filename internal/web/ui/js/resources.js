// 资源中心 tab module (DESIGN §4.12, ADR 0004/0005).
// Secret values are write-only: the page never receives plaintext, and the
// input is cleared as soon as it is submitted.
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, toast, confirmAction, setupDialog } from './utils.js';

export const resState = {
  kind: 'secret',
  view: 'live', // live | pending | takedowns
  resources: [],
  submissions: [],
  takedowns: [],
  peers: [],
  selected: new Set(),
  q: '',
  editing: null, // resource being edited, null = create
  grantFor: null, // resource whose grants dialog is open
  review: null, // submission being reviewed
};

export const RES_VIEWS = [
  { view: 'live', label: '正式资源' },
  { view: 'pending', label: '待审核' },
  { view: 'takedowns', label: '下架申请' },
];

export const SUBMISSION_STATUS = {
  pending: { label: '待审核', tone: 'warn' },
  approved: { label: '已批准', tone: 'ok' },
  rejected: { label: '已拒绝', tone: 'bad' },
  withdrawn: { label: '已撤回', tone: 'muted' },
  expired: { label: '已过期', tone: 'muted' },
  superseded: { label: '已被新提交取代', tone: 'muted' },
};

export const submissionStatusLabel = (s) => SUBMISSION_STATUS[s]?.label || s || '—';
export const submissionStatusTone = (s) => SUBMISSION_STATUS[s]?.tone || 'muted';
export const canBulkReview = (sub) => sub && sub.kind !== 'skill' && sub.status === 'pending';

export function filterSubmissions(list, q) {
  const s = (q || '').trim().toLowerCase();
  if (!s) return list;
  return list.filter((r) => (r.name + ' ' + (r.description || '') + ' ' + (r.submitted_by || '')).toLowerCase().includes(s));
}

export const ownerLabel = (r) => (r && r.owner_peer ? r.owner_peer : 'operator');

const RES_ROOT = '/admin/resources';
const SKILL_UPLOAD = '/admin/resources/skill';
export const MAX_SKILL_UPLOAD = 4 << 20;

// checkSkillFile rejects obviously wrong uploads before they leave the
// browser; the server does the real validation (frontmatter, paths, size).
export function checkSkillFile(file) {
  if (!file) return '请选择 skill 压缩包（.tar.gz / .tgz）';
  if (!/\.(tar\.gz|tgz)$/i.test(file.name)) return '只支持 .tar.gz / .tgz（包内含 SKILL.md）';
  if (file.size > MAX_SKILL_UPLOAD) return '压缩包超过 4 MiB';
  return '';
}

export const humanSize = (n) => (n >= 1 << 20 ? (n / (1 << 20)).toFixed(1) + ' MiB'
  : n >= 1024 ? Math.round(n / 1024) + ' KiB' : n + ' B');
export const ALL_PEERS = '*';

// Kinds the hub can store in this build. Others render as "即将支持".
export const RES_KINDS = [
  { kind: 'secret', label: '密钥', ready: true },
  { kind: 'mcp', label: 'MCP', ready: true },
  { kind: 'skill', label: 'Skill', ready: true },
  { kind: 'prompt', label: 'Prompt', ready: true },
];

export const kindLabel = (kind) => RES_KINDS.find((k) => k.kind === kind)?.label || kind;

export const MCP_EXAMPLE = {
  transport: 'stdio',
  command: 'npx',
  args: ['-y', '@modelcontextprotocol/server-github'],
  env: { GITHUB_TOKEN: '${secret:GITHUB_TOKEN}' },
};

// secretRefsIn lists ${secret:NAME} references in an MCP config text, sorted
// and de-duplicated (mirrors internal/resources secretRefRe).
export function secretRefsIn(text) {
  const out = new Set();
  for (const m of String(text || '').matchAll(/\$\{secret:([A-Za-z0-9][A-Za-z0-9_.-]{0,63})\}/g)) out.add(m[1]);
  return [...out].sort();
}

// buildPutBody turns the editor fields into the PUT body for a kind.
// Throws Error with a user-facing message on invalid input.
export function buildPutBody(kind, { description = '', value = '', content = '' }, editing) {
  const body = { description: description.trim() };
  if (kind === 'secret') {
    if (!editing && !value) throw new Error('请填写密钥值');
    body.value = value;
  } else if (kind === 'mcp') {
    try {
      body.content = JSON.parse(content);
    } catch (e) {
      throw new Error('MCP 配置不是合法 JSON：' + e.message);
    }
    if (!body.content || typeof body.content !== 'object' || Array.isArray(body.content)) {
      throw new Error('MCP 配置必须是 JSON 对象');
    }
  } else if (kind === 'prompt') {
    if (!content.trim()) throw new Error('请填写 Prompt 内容');
    body.content = content;
  }
  return body;
}

// Kept as concatenation so the console route contract test sees a real
// /admin/resources prefix.
export const resURL = (kind, name, tail = '') =>
  RES_ROOT + '/' + encodeURIComponent(kind) + '/' + encodeURIComponent(name) + (tail ? '/' + tail : '');

export const grantURL = (kind, name, peer) => resURL(kind, name, 'grants') + '/' + encodeURIComponent(peer);

export const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;

// grantSummary describes who can use a resource.
export function grantSummary(grants) {
  const g = grants || [];
  if (g.includes(ALL_PEERS)) return { text: '全部助手', tone: 'ok' };
  if (!g.length) return { text: '未授权', tone: 'muted' };
  return { text: g.length + ' 个助手', tone: 'info' };
}

// grantRows builds the grant dialog rows: every peer, its switch state, and
// whether the switch is shadowed by the wildcard grant.
export function grantRows(peers, grants) {
  const set = new Set(grants || []);
  const wildcard = set.has(ALL_PEERS);
  return (peers || []).map((p) => ({
    id: p.id,
    name: p.display_name || p.id,
    online: !!p.online,
    granted: set.has(p.id),
    viaWildcard: wildcard && !set.has(p.id),
  }));
}

export function filterResources(list, q) {
  const s = (q || '').trim().toLowerCase();
  if (!s) return list;
  return list.filter((r) => (r.name + ' ' + (r.description || '')).toLowerCase().includes(s));
}

async function loadPeers() {
  const r = await api('/admin/peers?page_size=200');
  resState.peers = r.peers || [];
}

export async function refreshResources() {
  const tbody = $('resTable')?.querySelector('tbody');
  if (!tbody) return;
  const isLive = resState.view === 'live';
  const kind = RES_KINDS.find((k) => k.kind === resState.kind);
  $('resCreate').hidden = !isLive || !kind?.ready;
  $('resBulkApprove').hidden = resState.view !== 'pending';
  $('resBulkReject').hidden = resState.view !== 'pending';
  $('resViewNote').textContent = resState.view === 'pending'
    ? '助手提交的资源；批准后才会进入正式目录。'
    : resState.view === 'takedowns' ? '来源助手申请下架；批准后删除资源并撤销全部授权。' : '';
  setResTableHead();
  if (resState.view === 'pending') return refreshSubmissions();
  if (resState.view === 'takedowns') return refreshTakedowns();
  refreshResourceBadges();
  if (!kind?.ready) {
    tbody.innerHTML = `<tr><td colspan="6" class="empty-td"><div class="empty">${icon('package')}<p>${esc(kind?.label || '')} 资源即将支持</p></div></td></tr>`;
    return;
  }
  tbody.innerHTML = `<tr class="skel"><td colspan="6"><i></i></td></tr>`;
  try {
    const r = await api(RES_ROOT + '?kind=' + encodeURIComponent(resState.kind));
    resState.resources = r.resources || [];
    renderResources();
  } catch (err) {
    if (err.status !== 401) {
      tbody.innerHTML = `<tr><td colspan="6" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
    }
  }
}

function setResTableHead() {
  const thead = $('resTable')?.querySelector('thead');
  if (!thead) return;
  if (resState.view === 'pending') {
    thead.innerHTML = '<tr><th class="col-check"><input type="checkbox" id="resSelectAll" aria-label="全选"></th><th>名称</th><th>描述</th><th>来源</th><th>状态</th><th>提交</th><th class="th-right">操作</th></tr>';
    return;
  }
  if (resState.view === 'takedowns') {
    thead.innerHTML = '<tr><th>名称</th><th>原因</th><th>申请人</th><th>状态</th><th>提交</th><th class="th-right">操作</th></tr>';
    return;
  }
  thead.innerHTML = '<tr><th>名称</th><th>描述</th><th>授权</th><th>来源</th><th>更新</th><th class="th-right">操作</th></tr>';
}

async function refreshResourceBadges() {
  try {
    const r = await api('/admin/resource-submissions?status=pending&page_size=1');
    updatePendingBadges(r);
    if (r.pending_takedowns == null) {
      const t = await api('/admin/resource-takedowns?status=pending&page_size=1');
      const el = $('resTakedownCount');
      if (el) { el.textContent = t.total || 0; el.hidden = !(t.total || 0); }
    }
  } catch { /* badge is advisory; ignore */ }
}

async function refreshSubmissions() {
  const tbody = $('resTable')?.querySelector('tbody');
  if (!tbody) return;
  resState.selected.clear();
  tbody.innerHTML = `<tr class="skel"><td colspan="7"><i></i></td></tr>`;
  try {
    const r = await api('/admin/resource-submissions?status=pending&page_size=200&kind=' + encodeURIComponent(resState.kind));
    resState.submissions = r.submissions || [];
    updatePendingBadges(r);
    renderSubmissions();
  } catch (err) {
    if (err.status !== 401) tbody.innerHTML = `<tr><td colspan="7" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
  }
}

async function refreshTakedowns() {
  const tbody = $('resTable')?.querySelector('tbody');
  if (!tbody) return;
  tbody.innerHTML = `<tr class="skel"><td colspan="6"><i></i></td></tr>`;
  try {
    const r = await api('/admin/resource-takedowns?status=pending&page_size=200');
    resState.takedowns = r.takedowns || [];
    renderTakedowns(r);
  } catch (err) {
    if (err.status !== 401) tbody.innerHTML = `<tr><td colspan="6" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
  }
}

function updatePendingBadges(r) {
  const subs = r?.pending_submissions ?? (resState.submissions || []).length;
  const tds = r?.pending_takedowns ?? (Array.isArray(r?.takedowns) ? (r.total || r.takedowns.length) : 0);
  for (const [id, n] of [['resPendingCount', subs], ['resTakedownCount', tds]]) {
    const el = $(id);
    if (!el) continue;
    el.textContent = n;
    el.hidden = !n;
  }
}

function statusBadgeFor(s) {
  const tone = submissionStatusTone(s);
  return `<span class="badge ${tone === 'muted' ? '' : tone}">${esc(submissionStatusLabel(s))}</span>`;
}

export function filterTakedowns(list, q) {
  const s = (q || '').trim().toLowerCase();
  if (!s) return list;
  return list.filter((t) => (t.name + ' ' + (t.reason || '') + ' ' + (t.requested_by || '')).toLowerCase().includes(s));
}

function renderResources() {
  const tbody = $('resTable')?.querySelector('tbody');
  if (!tbody) return;
  const list = filterResources(resState.resources, resState.q);
  if (!list.length) {
    tbody.innerHTML = `<tr><td colspan="6" class="empty-td"><div class="empty">${icon('lock')}<p>${resState.q ? '没有匹配的资源' : `还没有${esc(kindLabel(resState.kind))}，点「新建」登记一个`}</p></div></td></tr>`;
    return;
  }
  tbody.innerHTML = list.map((r) => {
    const g = grantSummary(r.grants);
    return `<tr>
      <td><code>${esc(r.name)}</code>${(r.requires_secrets || []).length ? `<br><small class="muted">依赖密钥：${r.requires_secrets.map(esc).join(', ')}</small>` : ''}${r.kind === 'skill' && r.size ? `<br><small class="muted">${esc(humanSize(r.size))}</small>` : ''}</td>
      <td class="muted">${esc(r.description || '—')}</td>
      <td><span class="badge ${g.tone === 'muted' ? '' : g.tone}">${esc(g.text)}</span></td>
      <td>${esc(ownerLabel(r))}</td>
      <td><div class="time-cell"><span>${fmtTime(r.updated_at)}</span><small>${esc(ago(r.updated_at))}</small></div></td>
      <td class="td-actions">
        <button class="btn-icon" data-act="grants" data-name="${esc(r.name)}" data-tip="授权" aria-label="授权">${icon('users')}</button>
        <button class="btn-icon" data-act="edit" data-name="${esc(r.name)}" data-tip="编辑 / 覆盖" aria-label="编辑">${icon('key')}</button>
        <button class="btn-icon danger" data-act="delete" data-name="${esc(r.name)}" data-tip="删除" aria-label="删除">${icon('trash')}</button>
      </td>
    </tr>`;
  }).join('');
  tbody.querySelectorAll('button[data-act]').forEach((b) => {
    const r = resState.resources.find((x) => x.name === b.dataset.name);
    if (!r) return;
    b.onclick = () => {
      if (b.dataset.act === 'grants') openGrants(r);
      else if (b.dataset.act === 'edit') openEditor(r);
      else deleteResource(r);
    };
  });
}

function renderSubmissions() {
  const tbody = $('resTable')?.querySelector('tbody');
  if (!tbody) return;
  const list = filterSubmissions(resState.submissions, resState.q);
  if (!list.length) {
    tbody.innerHTML = `<tr><td colspan="7" class="empty-td"><div class="empty">${icon('inbox')}<p>${resState.q ? '没有匹配的待审核提交' : '没有待审核的提交'}</p></div></td></tr>`;
    return;
  }
  tbody.innerHTML = list.map((sub) => {
    const bulk = canBulkReview(sub);
    return `<tr>
      <td class="col-check">${bulk ? `<input type="checkbox" data-id="${esc(sub.id)}" ${resState.selected.has(sub.id) ? 'checked' : ''} aria-label="选择 ${esc(sub.name)}">` : ''}</td>
      <td><code>${esc(sub.name)}</code><br><small class="muted">${esc(sub.kind)} · ${esc(shortHash(sub.hash))}</small></td>
      <td class="muted">${esc(sub.description || '—')}</td>
      <td>${esc(sub.submitted_by)}</td>
      <td>${statusBadgeFor(sub.status)}</td>
      <td><div class="time-cell"><span>${fmtTime(sub.created_at)}</span><small>${esc(ago(sub.created_at))}</small></div></td>
      <td class="td-actions">
        <button class="btn btn-outline btn-xs" data-act="review" data-id="${esc(sub.id)}">审核</button>
      </td>
    </tr>`;
  }).join('');
  tbody.querySelectorAll('input[data-id]').forEach((cb) => {
    cb.onchange = () => {
      if (cb.checked) resState.selected.add(cb.dataset.id);
      else resState.selected.delete(cb.dataset.id);
    };
  });
  tbody.querySelectorAll('button[data-act="review"]').forEach((b) => {
    b.onclick = () => openReview(b.dataset.id);
  });
  const all = $('resSelectAll');
  if (all) {
    all.checked = false;
    all.onchange = () => {
      resState.selected.clear();
      if (all.checked) list.filter(canBulkReview).forEach((s) => resState.selected.add(s.id));
      renderSubmissions();
    };
  }
}

function renderTakedowns(r) {
  const tbody = $('resTable')?.querySelector('tbody');
  if (!tbody) return;
  updatePendingBadges(r);
  const list = filterTakedowns(resState.takedowns, resState.q);
  if (!list.length) {
    tbody.innerHTML = `<tr><td colspan="6" class="empty-td"><div class="empty">${icon('inbox')}<p>${resState.q ? '没有匹配的下架申请' : '没有待处理的下架申请'}</p></div></td></tr>`;
    return;
  }
  tbody.innerHTML = list.map((t) => `<tr>
    <td><code>${esc(t.name)}</code><br><small class="muted">${esc(t.kind)}</small></td>
    <td class="muted">${esc(t.reason || '—')}</td>
    <td>${esc(t.requested_by)}</td>
    <td>${statusBadgeFor(t.status)}</td>
    <td><div class="time-cell"><span>${fmtTime(t.created_at)}</span><small>${esc(ago(t.created_at))}</small></div></td>
    <td class="td-actions">
      <button class="btn btn-outline btn-xs" data-act="td-approve" data-id="${esc(t.id)}">批准下架</button>
      <button class="btn btn-ghost btn-xs" data-act="td-reject" data-id="${esc(t.id)}">拒绝</button>
    </td>
  </tr>`).join('');
  tbody.querySelectorAll('button[data-act^="td-"]').forEach((b) => {
    b.onclick = () => resolveTakedown(b.dataset.id, b.dataset.act === 'td-approve');
  });
}

async function openReview(id) {
  resState.review = null;
  const err = $('resReviewErr');
  err.hidden = true;
  $('resReviewBody').innerHTML = '<p class="muted small">加载中…</p>';
  $('resReviewSkillAckRow').hidden = true;
  $('resReviewSkillAck').checked = false;
  try {
    const r = await api('/admin/resource-submissions/' + encodeURIComponent(id));
    const d = r.submission;
    resState.review = d;
    $('resReviewTitle').textContent = `审核 ${d.kind} · ${d.name}`;
    $('resReviewMeta').textContent = `来源 ${d.submitted_by} · 提交于 ${fmtTime(d.created_at)}`;
    const isSkill = d.kind === 'skill';
    const isSecret = d.kind === 'secret';
    $('resReviewNameField').hidden = isSkill;
    $('resReviewName').value = d.name;
    $('resReviewDesc').value = d.description || '';
    if (isSkill) {
      const insp = d.skill || { files: [], skill_md: '' };
      $('resReviewBody').innerHTML = `<div class="alert alert-warn"><svg class="icon"><use href="#i-alert"/></svg><span>Skill 会被助手执行。请核对文件清单与 SKILL.md；中继本身不执行。</span></div>
        <p class="muted xs">文件（${insp.files_count || insp.files.length}）：${insp.files.map(esc).join(', ')}</p>
        <pre class="log" style="max-height:220px">${esc(insp.skill_md || '')}</pre>
        <button type="button" class="btn btn-outline btn-xs" id="resSkillDownload">下载 tar.gz</button>`;
      $('resReviewSkillAckRow').hidden = false;
      $('resSkillDownload').onclick = () => { location.href = '/admin/resource-submissions/' + encodeURIComponent(id) + '/archive'; };
    } else if (isSecret) {
      $('resReviewBody').innerHTML = `<div class="alert alert-info"><svg class="icon"><use href="#i-info"/></svg><span>密钥明文永不在控制台显示。请根据来源、名称和描述判断；需要验证时让来源助手说明用途，或先只授权给它自己试用。</span></div>`;
    } else {
      const body = d.kind === 'mcp' ? JSON.stringify(d.content, null, 2) : String(d.content || '');
      $('resReviewBody').innerHTML = `<pre class="log" style="max-height:260px">${esc(body)}</pre>`;
    }
    $('resReviewDlg').showModal();
  } catch (e) {
    toast('加载提交失败：' + e.message, 'bad');
  }
}

async function approveReview() {
  const d = resState.review;
  if (!d) return;
  const err = $('resReviewErr');
  err.hidden = true;
  if (d.kind === 'skill' && !$('resReviewSkillAck').checked) {
    err.textContent = '请先勾选「我已查看该 skill」';
    err.hidden = false;
    return;
  }
  const body = { description: $('resReviewDesc').value };
  if (d.kind !== 'skill') body.name = $('resReviewName').value.trim();
  try {
    await api('/admin/resource-submissions/' + encodeURIComponent(d.id) + '/approve', { method: 'POST', body: JSON.stringify(body) });
    toast(`已批准 ${d.name}`, 'ok');
    $('resReviewDlg').close();
    refreshResources();
  } catch (e) {
    err.textContent = e.message;
    err.hidden = false;
  }
}

function rejectReview() {
  const d = resState.review;
  if (!d) return;
  $('resReviewDlg').close();
  openReject(d.id);
}

async function openReject(id) {
  $('resRejectNote').value = '';
  $('resRejectDlg').dataset.id = id;
  $('resRejectDlg').showModal();
}

async function submitReject() {
  const id = $('resRejectDlg').dataset.id;
  if (!id) return;
  try {
    await api('/admin/resource-submissions/' + encodeURIComponent(id) + '/reject', { method: 'POST', body: JSON.stringify({ note: $('resRejectNote').value.trim() }) });
    toast('已拒绝提交', 'ok');
    $('resRejectDlg').close();
    refreshResources();
  } catch (e) {
    toast('拒绝失败：' + e.message, 'bad');
  }
}

async function bulkReview(action) {
  const ids = [...resState.selected];
  if (!ids.length) return toast('先勾选要处理的提交', 'warn');
  try {
    const r = await api('/admin/resource-submissions/bulk', { method: 'POST', body: JSON.stringify({ ids, action }) });
    const results = r.results || [];
    const ok = results.filter((x) => x.ok).length;
    const bad = results.filter((x) => !x.ok);
    toast(`批量处理完成：成功 ${ok} 个${bad.length ? `，失败 ${bad.length} 个` : ''}`, bad.length ? 'warn' : 'ok', 6000);
    resState.selected.clear();
    refreshResources();
  } catch (e) {
    toast('批量处理失败：' + e.message, 'bad');
  }
}

async function resolveTakedown(id, approve) {
  let note = '';
  if (!approve) note = window.prompt('拒绝原因（可选）', '') || '';
  try {
    await api('/admin/resource-takedowns/' + encodeURIComponent(id) + '/' + (approve ? 'approve' : 'reject'), { method: 'POST', body: JSON.stringify({ note }) });
    toast(approve ? '已批准下架' : '已拒绝下架申请', 'ok');
    refreshResources();
  } catch (e) {
    toast('处理失败：' + e.message, 'bad');
  }
}

function shortHash(h) {
  return h && h.length > 16 ? h.slice(0, 16) : (h || '');
}

function openEditor(r = null) {
  resState.editing = r;
  const editing = !!r;
  const kind = resState.kind;
  const label = kindLabel(kind);
  const isSecret = kind === 'secret';
  const isSkill = kind === 'skill';
  $('resDlgTitle').textContent = editing ? `${isSkill ? '更新' : '编辑'}${label} ${r.name}` : `${isSkill ? '上传' : '新建'}${label}`;
  $('resDlgDesc').textContent = isSecret ? '服务端 AES-GCM 加密存储，永不回显'
    : kind === 'mcp' ? '中立格式：transport 为 stdio（command/args/env）或 http（url/headers）；值里可用 ${secret:NAME} 引用密钥'
    : isSkill ? 'SKILL.md 所在目录打成 .tar.gz（≤1 MiB 解压后）；名称取自 SKILL.md 的 name，描述默认取 description'
    : '下发给助手的业务 Prompt 片段（与 relay 自身的工作指令无关）';
  $('resName').value = editing ? r.name : '';
  $('resName').readOnly = editing;
  $('resNameField').hidden = isSkill;
  $('resName').required = !isSkill;
  $('resDesc').value = editing ? (r.description || '') : '';
  $('resDesc').placeholder = isSkill ? '留空则使用 SKILL.md 里的 description' : '用途、来源账号、轮换周期';
  $('resValueField').hidden = !isSecret;
  $('resContentField').hidden = isSecret || isSkill;
  $('resFileField').hidden = !isSkill;
  $('resFile').value = '';
  $('resValue').value = '';
  $('resValue').placeholder = editing ? '留空则保留原值；填写即覆盖' : '粘贴密钥明文';
  const content = $('resContent');
  content.classList.toggle('mono', kind === 'mcp');
  if (kind === 'mcp') content.value = JSON.stringify(editing ? r.content : MCP_EXAMPLE, null, 2);
  else content.value = editing ? (r.content || '') : '';
  $('resDlgErr').hidden = true;
  $('resDlg').showModal();
}

async function submitEditor(e) {
  e.preventDefault();
  const name = $('resName').value.trim();
  const err = $('resDlgErr');
  const fail = (m) => { err.textContent = m; err.hidden = false; };
  if (resState.kind === 'skill') return submitSkill(fail);
  if (!NAME_RE.test(name)) return fail('名称只能用字母、数字、_ . -，以字母或数字开头，最多 64 个字符');
  const kind = resState.kind;
  const value = $('resValue').value;
  let body;
  try {
    body = buildPutBody(kind, { description: $('resDesc').value, value, content: $('resContent').value }, !!resState.editing);
  } catch (e2) {
    return fail(e2.message);
  }
  // Clear secret plaintext from the DOM before the request goes out.
  $('resValue').value = '';
  try {
    await api(resURL(kind, name), { method: 'PUT', body: JSON.stringify(body) });
    const verb = resState.editing ? (kind === 'secret' && !value ? '已更新' : '已保存') : '已登记';
    toast(`${verb} ${name}`, 'ok');
    $('resDlg').close();
    refreshResources();
  } catch (e2) {
    fail(e2.message);
  }
}

async function submitSkill(fail) {
  const file = $('resFile').files?.[0];
  const bad = checkSkillFile(file);
  if (bad) return fail(bad);
  const form = new FormData();
  form.append('archive', file);
  const desc = $('resDesc').value.trim();
  if (desc) form.append('description', desc);
  try {
    // multipart: no JSON content type (the browser sets the boundary).
    const r = await fetch(SKILL_UPLOAD, { method: 'POST', body: form });
    const body = await r.json().catch(() => null);
    if (!r.ok) throw new Error((body && body.error) || ('HTTP ' + r.status));
    const v = body.resource;
    if (resState.editing && v.name !== resState.editing.name) {
      toast(`压缩包里的 SKILL.md 名为 ${v.name}，已另存为新 skill（不是更新 ${resState.editing.name}）`, 'warn', 7000);
    } else {
      toast(`已上传 skill ${v.name}`, 'ok');
    }
    $('resDlg').close();
    refreshResources();
  } catch (e2) {
    fail(e2.message);
  }
}

async function deleteResource(r) {
  const ok = await confirmAction({
    title: `删除 ${r.name}？`,
    body: '删除后所有助手都拉取不到它，清单会随之更新。' + (r.kind === 'secret'
      ? '\n已授权的助手本地可能还有副本，建议到外部服务把这个密钥轮换掉。'
      : '\n助手会按清单删除本地副本。'),
    danger: true,
    okText: '确认删除',
  });
  if (!ok) return;
  try {
    await api(resURL(r.kind, r.name), { method: 'DELETE' });
    toast(`已删除 ${r.name}`, 'ok');
    refreshResources();
  } catch (err) {
    toast('删除失败：' + err.message, 'bad');
  }
}

async function openGrants(r) {
  resState.grantFor = r;
  $('grantTitle').textContent = `授权 ${r.name}`;
  try {
    await loadPeers();
  } catch (err) {
    toast('无法读取成员列表：' + err.message, 'bad');
    return;
  }
  renderGrants();
  $('grantDlg').showModal();
}

function renderGrants() {
  const r = resState.grantFor;
  if (!r) return;
  const wildcard = (r.grants || []).includes(ALL_PEERS);
  $('grantAll').checked = wildcard;
  const rows = grantRows(resState.peers, r.grants);
  const box = $('grantList');
  box.innerHTML = rows.length ? rows.map((p) => `
    <label class="switch grant-row">
      <input type="checkbox" data-peer="${esc(p.id)}" ${p.granted || p.viaWildcard ? 'checked' : ''} ${p.viaWildcard ? 'disabled' : ''}>
      <span></span>${esc(p.name)} <small class="muted">${esc(p.id)}${p.online ? ' · 在线' : ''}</small>
    </label>`).join('') : `<p class="muted small">还没有注册的助手</p>`;
  box.querySelectorAll('input[data-peer]').forEach((cb) => {
    cb.onchange = () => setGrant(cb.dataset.peer, cb.checked, cb);
  });
}

async function setGrant(peer, on, input) {
  const r = resState.grantFor;
  if (!r) return;
  if (!on) {
    const ok = await confirmAction({
      title: `撤销 ${peer === ALL_PEERS ? '全部助手' : peer} 的授权？`,
      body: '撤销后该助手的清单会更新，但它本地可能还有副本。' + (r.kind === 'secret' ? '\n建议到外部服务轮换这个密钥。' : ''),
      danger: true,
      okText: '确认撤销',
    });
    if (!ok) {
      if (input) input.checked = true;
      return;
    }
  }
  try {
    if (on) {
      const res = await api(resURL(r.kind, r.name, 'grants'), { method: 'POST', body: JSON.stringify({ peer_id: peer }) });
      r.grants = [...new Set([...(r.grants || []), peer])];
      const missing = res?.missing_secrets || [];
      if (missing.length) {
        toast(`已授权，但 ${peer === ALL_PEERS ? '全部助手' : peer} 还缺少依赖密钥：${missing.join(', ')}；拉取时会返回 409`, 'warn', 7000);
      }
    } else {
      await api(grantURL(r.kind, r.name, peer), { method: 'DELETE' });
      r.grants = (r.grants || []).filter((g) => g !== peer);
    }
    renderGrants();
    renderResources();
  } catch (err) {
    if (input) input.checked = !on;
    toast('授权失败：' + err.message, 'bad');
  }
}

export function initResources() {
  for (const id of ['resDlg', 'grantDlg', 'resReviewDlg', 'resRejectDlg']) setupDialog($(id));
  const form = $('resForm');
  if (form) form.onsubmit = submitEditor;
  $('resDlg')?.addEventListener('close', () => { $('resValue').value = ''; $('resContent').value = ''; resState.editing = null; });
  $('grantDlg')?.addEventListener('close', () => { resState.grantFor = null; });
  $('resReviewDlg')?.addEventListener('close', () => { resState.review = null; });
  const all = $('grantAll');
  if (all) all.onchange = () => setGrant(ALL_PEERS, all.checked, all);
  const create = $('resCreate');
  if (create) create.onclick = () => openEditor(null);
  const refresh = $('resRefresh');
  if (refresh) refresh.onclick = refreshResources;
  const reviewApprove = $('resReviewApprove');
  if (reviewApprove) reviewApprove.onclick = approveReview;
  const reviewReject = $('resReviewReject');
  if (reviewReject) reviewReject.onclick = rejectReview;
  const rejectOk = $('resRejectOk');
  if (rejectOk) rejectOk.onclick = submitReject;
  const bulkApprove = $('resBulkApprove');
  if (bulkApprove) bulkApprove.onclick = () => bulkReview('approve');
  const bulkReject = $('resBulkReject');
  if (bulkReject) bulkReject.onclick = () => bulkReview('reject');
  const viewSeg = $('resViewSeg');
  if (viewSeg) {
    viewSeg.querySelectorAll('button').forEach((b) => {
      b.onclick = () => {
        viewSeg.querySelectorAll('button').forEach((x) => x.classList.remove('on'));
        b.classList.add('on');
        resState.view = b.dataset.v;
        resState.q = '';
        const q = $('resQ'); if (q) q.value = '';
        refreshResources();
      };
    });
  }
  const seg = $('resSeg');
  if (seg) {
    seg.querySelectorAll('button').forEach((b) => {
      b.onclick = () => {
        seg.querySelectorAll('button').forEach((x) => x.classList.remove('on'));
        b.classList.add('on');
        resState.kind = b.dataset.v;
        refreshResources();
      };
    });
  }
  const q = $('resQ');
  if (q) q.oninput = () => {
    resState.q = q.value;
    if (resState.view === 'pending') renderSubmissions();
    else if (resState.view === 'takedowns') renderTakedowns();
    else renderResources();
  };
}
