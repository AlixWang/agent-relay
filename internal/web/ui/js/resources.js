// 资源中心 tab module (DESIGN §4.12, ADR 0004/0005).
// Secret values are write-only: the page never receives plaintext, and the
// input is cleared as soon as it is submitted.
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, toast, confirmAction, setupDialog } from './utils.js';

export const resState = {
  kind: 'secret',
  resources: [],
  peers: [],
  q: '',
  editing: null, // resource being edited, null = create
  grantFor: null, // resource whose grants dialog is open
};

const RES_ROOT = '/admin/resources';
export const ALL_PEERS = '*';

// Kinds the hub can store in this build. Others render as "即将支持".
export const RES_KINDS = [
  { kind: 'secret', label: '密钥', ready: true },
  { kind: 'mcp', label: 'MCP', ready: true },
  { kind: 'skill', label: 'Skill', ready: false },
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
  const kind = RES_KINDS.find((k) => k.kind === resState.kind);
  $('resCreate').hidden = !kind?.ready;
  if (!kind?.ready) {
    tbody.innerHTML = `<tr><td colspan="5" class="empty-td"><div class="empty">${icon('package')}<p>${esc(kind?.label || '')} 资源即将支持</p></div></td></tr>`;
    return;
  }
  tbody.innerHTML = `<tr class="skel"><td colspan="5"><i></i></td></tr>`;
  try {
    const r = await api(RES_ROOT + '?kind=' + encodeURIComponent(resState.kind));
    resState.resources = r.resources || [];
    renderResources();
  } catch (err) {
    if (err.status !== 401) {
      tbody.innerHTML = `<tr><td colspan="5" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
    }
  }
}

function renderResources() {
  const tbody = $('resTable')?.querySelector('tbody');
  if (!tbody) return;
  const list = filterResources(resState.resources, resState.q);
  if (!list.length) {
    tbody.innerHTML = `<tr><td colspan="5" class="empty-td"><div class="empty">${icon('lock')}<p>${resState.q ? '没有匹配的资源' : `还没有${esc(kindLabel(resState.kind))}，点「新建」登记一个`}</p></div></td></tr>`;
    return;
  }
  tbody.innerHTML = list.map((r) => {
    const g = grantSummary(r.grants);
    return `<tr>
      <td><code>${esc(r.name)}</code>${(r.requires_secrets || []).length ? `<br><small class="muted">依赖密钥：${r.requires_secrets.map(esc).join(', ')}</small>` : ''}</td>
      <td class="muted">${esc(r.description || '—')}</td>
      <td><span class="badge ${g.tone === 'muted' ? '' : g.tone}">${esc(g.text)}</span></td>
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

function openEditor(r = null) {
  resState.editing = r;
  const editing = !!r;
  const kind = resState.kind;
  const label = kindLabel(kind);
  const isSecret = kind === 'secret';
  $('resDlgTitle').textContent = editing ? `编辑${label} ${r.name}` : `新建${label}`;
  $('resDlgDesc').textContent = isSecret ? '服务端 AES-GCM 加密存储，永不回显'
    : kind === 'mcp' ? '中立格式：transport 为 stdio（command/args/env）或 http（url/headers）；值里可用 ${secret:NAME} 引用密钥'
    : '下发给助手的业务 Prompt 片段（与 relay 自身的工作指令无关）';
  $('resName').value = editing ? r.name : '';
  $('resName').readOnly = editing;
  $('resDesc').value = editing ? (r.description || '') : '';
  $('resValueField').hidden = !isSecret;
  $('resContentField').hidden = isSecret;
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
  for (const id of ['resDlg', 'grantDlg']) setupDialog($(id));
  const form = $('resForm');
  if (form) form.onsubmit = submitEditor;
  $('resDlg')?.addEventListener('close', () => { $('resValue').value = ''; $('resContent').value = ''; resState.editing = null; });
  $('grantDlg')?.addEventListener('close', () => { resState.grantFor = null; });
  const all = $('grantAll');
  if (all) all.onchange = () => setGrant(ALL_PEERS, all.checked, all);
  const create = $('resCreate');
  if (create) create.onclick = () => openEditor(null);
  const refresh = $('resRefresh');
  if (refresh) refresh.onclick = refreshResources;
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
  if (q) q.oninput = () => { resState.q = q.value; renderResources(); };
}
