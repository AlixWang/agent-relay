// Agent runtime log page: local receiver / watchdog / wake-chain diagnostics.
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, avatar, renderPager, toast } from './utils.js';

let logTimer = null;

export const logState = {
  page: 1,
  pageSize: 50,
  peer: '',
  level: '',
  source: '',
  event: '',
  q: '',
  range: '24h',
  thread: '',
};

export function logFilterParams(state = logState) {
  const params = new URLSearchParams();
  params.set('page', String(state.page));
  params.set('page_size', String(state.pageSize));
  params.set('facets', '1');
  if (state.peer) params.set('peer', state.peer);
  if (state.level) params.set('level', state.level);
  if (state.source) params.set('source', state.source);
  if (state.event) params.set('event', state.event);
  if (state.q) params.set('q', state.q);
  if (state.range) params.set('range', state.range);
  if (state.thread) params.set('thread', state.thread);
  return params;
}

export function logLevelTone(level) {
  switch (level) {
    case 'error': return 'bad';
    case 'warn': return 'warn';
    case 'info': return 'mono';
    default: return 'muted';
  }
}

function readHashFilters() {
  const raw = location.hash.includes('?') ? location.hash.split('?')[1] : '';
  const q = new URLSearchParams(raw);
  if (q.has('peer')) logState.peer = q.get('peer') || '';
  if (q.has('thread')) logState.thread = q.get('thread') || '';
  if (q.has('level')) logState.level = q.get('level') || '';
  if (q.has('event')) logState.event = q.get('event') || '';
  if (q.has('range')) logState.range = q.get('range') || '24h';
}

// Route entry: reset filters, then apply the hash query so links from the
// members/workbench tables open the right context.
export function enterLogs() {
  logState.page = 1;
  logState.peer = '';
  logState.level = '';
  logState.source = '';
  logState.event = '';
  logState.q = '';
  logState.thread = '';
  logState.range = '24h';
  readHashFilters();
  for (const [id, value] of [
    ['logPeer', logState.peer], ['logLevel', logState.level],
    ['logSource', logState.source], ['logEvent', logState.event],
    ['logQ', logState.q], ['logRange', logState.range],
  ]) {
    const el = $(id);
    if (el) el.value = value;
  }
  loadPeerOptions();
  refreshLogs();
  clearInterval(logTimer);
  logTimer = setInterval(() => {
    if (document.visibilityState === 'visible') refreshLogs(true);
  }, 5000);
}

async function loadPeerOptions() {
  const sel = $('logPeer');
  if (!sel || sel.dataset.loaded === '1') return;
  try {
    const r = await api('/admin/peers?page_size=200');
    const peers = r.peers || [];
    sel.innerHTML = '<option value="">全部成员</option>' + peers.map((p) =>
      `<option value="${esc(p.id)}">${esc(p.id)}${p.display_name ? ' · ' + esc(p.display_name) : ''}</option>`).join('');
    if (logState.peer) sel.value = logState.peer;
    sel.dataset.loaded = '1';
  } catch (_) {
    // Filter population is decoration; the page still loads.
  }
}

function renderStats(stats) {
  const el = $('logStats');
  if (!el) return;
  const s = stats || {};
  el.innerHTML = [
    `<span class="badge bad">24h error ${s.errors || 0}</span>`,
    `<span class="badge warn">24h warn ${s.warnings || 0}</span>`,
    `<span class="badge muted">${s.peers || 0} 个成员上报</span>`,
    s.last_received_at ? `<span class="badge mono">最后 ${esc(ago(s.last_received_at))}</span>` : '',
  ].join('');
}

function renderSources(sources) {
  const sel = $('logSource');
  if (!sel || !Array.isArray(sources)) return;
  const cur = logState.source;
  sel.innerHTML = '<option value="">全部来源</option>' + sources.map((s) =>
    `<option value="${esc(s)}" ${s === cur ? 'selected' : ''}>${esc(s)}</option>`).join('');
}

function renderRows(entries) {
  const table = $('logTable');
  if (!table) return;
  const tbody = table.querySelector('tbody');
  if (!tbody) return;
  if (!entries.length) {
    tbody.innerHTML = `<tr><td colspan="7" class="empty-td"><div class="empty">${icon('terminal')}<p>暂无 Agent 日志。若成员仍在运行旧版 <code>relay-watch.sh</code>，请重新下载 <code>/clients/relay-watch.sh</code> 并重启 hook。</p></div></td></tr>`;
    return;
  }
  tbody.innerHTML = entries.map((e) => {
    const task = [e.task_id, e.thread].filter(Boolean).map((v) =>
      `<code>${esc(v)}</code>`).join(' ');
    const fields = JSON.stringify(e.fields || {}, null, 2);
    return `<tr class="log-row" data-seq="${e.seq}">
      <td><div class="time-cell"><span>${fmtTime(e.ts || e.received_at)}</span><small>${esc(ago(e.received_at || e.ts))}</small></div></td>
      <td><span class="badge ${logLevelTone(e.level)}">${esc(e.level)}</span></td>
      <td><div class="who">${avatar(e.peer_id, 'sm')}<b>${esc(e.peer_id)}</b></div></td>
      <td><span class="badge mono">${esc(e.source)}</span></td>
      <td><code>${esc(e.event)}</code></td>
      <td><div class="kv-detail">${esc(e.message || '—')}</div></td>
      <td>${task || '<span class="muted xs">—</span>'}</td>
    </tr>
    <tr class="log-fields" data-for="${e.seq}" hidden><td colspan="7"><div class="log-json-head"><span>fields</span><button class="btn btn-ghost btn-xs" data-copy-json="${esc(fields)}">复制</button></div><pre>${esc(fields)}</pre></td></tr>`;
  }).join('');
  tbody.querySelectorAll('tr.log-row').forEach((tr) => {
    tr.onclick = () => {
      const detail = tbody.querySelector(`tr.log-fields[data-for="${tr.dataset.seq}"]`);
      if (detail) detail.hidden = !detail.hidden;
    };
  });
  tbody.querySelectorAll('button[data-copy-json]').forEach((btn) => {
    btn.onclick = async (e) => {
      e.stopPropagation();
      try {
        await navigator.clipboard.writeText(btn.dataset.copyJson || '');
        toast('已复制 fields', 'ok');
      } catch (_) {
        toast('复制失败，请手动选择', 'bad');
      }
    };
  });
}

export async function refreshLogs(silent = false) {
  const table = $('logTable');
  if (!table) return;
  const tbody = table.querySelector('tbody');
  if (!tbody) return;
  if (!silent) tbody.innerHTML = `<tr class="skel"><td colspan="7"><i></i></td></tr>`;
  try {
    const r = await api('/admin/logs?' + logFilterParams().toString());
    renderStats(r.stats);
    if (r.facets) {
      renderSources(r.facets.sources);
      const levelSel = $('logLevel');
      if (levelSel) levelSel.value = logState.level;
    }
    renderRows(r.entries || []);
    renderPager($('logPager'), {
      total: r.total || 0,
      page: r.page || 1,
      pageSize: r.page_size || logState.pageSize,
      onPage: (p) => { logState.page = p; refreshLogs(); },
    });
  } catch (err) {
    if (err.status === 401) return;
    tbody.innerHTML = `<tr><td colspan="7" class="empty-td"><div class="empty bad">${icon('err')}<p>加载失败：${esc(err.message)}</p></div></td></tr>`;
    if (!silent) toast('Agent 日志加载失败：' + err.message, 'bad');
  }
}

function exportLogs() {
  const params = logFilterParams();
  params.delete('page');
  params.delete('page_size');
  params.delete('facets');
  window.open('/admin/logs/export?' + params.toString(), '_blank', 'noopener');
  toast('已开始导出 Agent 日志', 'ok');
}

export function initLogs() {
  const peer = $('logPeer');
  if (peer) peer.onchange = (e) => {
    logState.peer = e.target.value;
    logState.page = 1;
    refreshLogs();
  };
  const level = $('logLevel');
  if (level) level.onchange = (e) => {
    logState.level = e.target.value;
    logState.page = 1;
    refreshLogs();
  };
  const source = $('logSource');
  if (source) source.onchange = (e) => {
    logState.source = e.target.value;
    logState.page = 1;
    refreshLogs();
  };
  const range = $('logRange');
  if (range) range.onchange = (e) => {
    logState.range = e.target.value;
    logState.page = 1;
    refreshLogs();
  };
  const ev = $('logEvent');
  let eventTimer = null;
  if (ev) ev.oninput = (e) => {
    clearTimeout(eventTimer);
    eventTimer = setTimeout(() => {
      logState.event = e.target.value.trim();
      logState.page = 1;
      refreshLogs();
    }, 300);
  };
  const q = $('logQ');
  let timer = null;
  if (q) q.oninput = (e) => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      logState.q = e.target.value.trim();
      logState.page = 1;
      refreshLogs();
    }, 300);
  };
  const refresh = $('refreshLogs');
  if (refresh) refresh.onclick = () => refreshLogs();
  const exportBtn = $('exportLogs');
  if (exportBtn) exportBtn.onclick = exportLogs;
}

export function stopLogs() {
  clearInterval(logTimer);
  logTimer = null;
}

