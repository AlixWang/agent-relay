// Threads and messages tab module
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, avatar, toast, confirmAction, renderPager, copyToClipboard, setupDialog, msgPayloadMap } from './utils.js';
import { refreshStats } from './auth.js';

export const threadState = {
  mode: 'threads', // 'threads' | 'search'
  state: '',       // '' | 'held' | 'fused'
  q: '',
  page: 1,
  pageSize: 20,
};

export async function refreshThreads() {
  if (threadState.mode === 'search') {
    refreshSearch();
    return;
  }
  const table = $('threadTable');
  if (!table) return;
  const tbody = table.querySelector('tbody');
  if (!tbody) return;
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

export async function refreshSearch() {
  const container = $('searchList');
  if (!container) return;
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
          <button class="btn btn-ghost btn-xs" data-view-thread="${esc(m.thread)}">${icon('eye', 'xs')}查看线程</button>
        </div>
        <div class="msg-body clamp">${esc(m.payload)}</div>
        <div class="msg-foot">
          <span>thread: <code>${esc(m.thread)}</code></span>
          <span>id: <code>${esc(m.id)}</code></span>
        </div>
      </div>`;
    }).join('');

    container.querySelectorAll('button[data-view-thread]').forEach((btn) => {
      btn.onclick = () => viewThread(btn.dataset.viewThread);
    });

    container.querySelectorAll('button[data-copy-msg]').forEach((btn) => {
      btn.onclick = async () => {
        const payload = msgPayloadMap.get(Number(btn.dataset.copyMsg));
        if (payload) {
          const ok = await copyToClipboard(payload);
          toast(ok ? '已复制消息内容' : '复制失败', ok ? 'ok' : 'bad');
        }
      };
    });

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

export async function viewThread(root) {
  const drawer = $('threadDrawer');
  if (!drawer) return;
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

  const copyMd = $('copyMd'); if (copyMd) copyMd.onclick = () => copyThreadExport('markdown');
  const copyJsonl = $('copyJsonl'); if (copyJsonl) copyJsonl.onclick = () => copyThreadExport('jsonl');
  const expMd = $('expMd');
  if (expMd) {
    expMd.onclick = () => {
      const expDropdown = $('expDropdown'); if (expDropdown) expDropdown.open = false;
      window.location = '/admin/messages/export?thread=' + encodeURIComponent(root) + '&format=' + format;
    };
  }
  const expJsonl = $('expJsonl');
  if (expJsonl) {
    expJsonl.onclick = () => {
      const expDropdown = $('expDropdown'); if (expDropdown) expDropdown.open = false;
      window.location = '/admin/messages/export?thread=' + encodeURIComponent(root) + '&format=jsonl';
    };
  }

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

    body.querySelectorAll('button[data-copy-msg]').forEach((btn) => {
      btn.onclick = async () => {
        const payload = msgPayloadMap.get(Number(btn.dataset.copyMsg));
        if (payload) {
          const ok = await copyToClipboard(payload);
          toast(ok ? '已复制消息内容' : '复制失败', ok ? 'ok' : 'bad');
        }
      };
    });

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

export function initThreads() {
  const threadMode = $('threadMode');
  if (threadMode) {
    threadMode.querySelectorAll('button').forEach((b) => {
      b.onclick = () => {
        threadMode.querySelectorAll('button').forEach((x) => x.classList.remove('on'));
        b.classList.add('on');
        threadState.mode = b.dataset.v;
        threadState.page = 1;
        const isSearch = threadState.mode === 'search';
        if ($('threadListView')) $('threadListView').hidden = isSearch;
        if ($('searchView')) $('searchView').hidden = !isSearch;
        if ($('threadState')) $('threadState').hidden = isSearch;
        const threadQ = $('threadQ');
        if (threadQ) threadQ.placeholder = isSearch ? '搜索 Payload 全文内容…' : '按 root_id / 参与者过滤';
        refreshThreads();
      };
    });
  }

  const threadStateEl = $('threadState');
  if (threadStateEl) {
    threadStateEl.querySelectorAll('button').forEach((b) => {
      b.onclick = () => {
        threadStateEl.querySelectorAll('button').forEach((x) => x.classList.remove('on'));
        b.classList.add('on');
        threadState.state = b.dataset.v;
        threadState.page = 1;
        refreshThreads();
      };
    });
  }

  const threadSearchForm = $('threadSearchForm');
  if (threadSearchForm) {
    threadSearchForm.onsubmit = (e) => {
      e.preventDefault();
      threadState.q = $('threadQ').value.trim();
      threadState.page = 1;
      refreshThreads();
    };
  }

  const threadQ = $('threadQ');
  let threadQTimer = null;
  if (threadQ) {
    threadQ.oninput = (e) => {
      clearTimeout(threadQTimer);
      threadQTimer = setTimeout(() => {
        threadState.q = e.target.value.trim();
        threadState.page = 1;
        refreshThreads();
      }, 350);
    };
  }

  const refreshThreadsBtn = $('refreshThreads');
  if (refreshThreadsBtn) refreshThreadsBtn.onclick = refreshThreads;
}
