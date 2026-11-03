// Work queue page: the attention-first view over projected tickets.
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, renderPager, toast } from './utils.js';
import { viewThread } from './threads.js';

export const workState = {
  filter: 'attention', // attention | working | done | all
  q: '',
  page: 1,
  pageSize: 20,
};

// Labels/tones are presentation only. Ordering comes from the server
// (internal/stages.OrderCase) so this table must never define a weight.
export const STAGE_META = {
  'person/approval': { label: '待审批', tone: 'warn' },
  'person/answer': { label: '待答复', tone: 'warn' },
  'person/review': { label: '待验收', tone: 'warn' },
  'person/blocked': { label: '已阻塞', tone: 'bad' },
  'person/failed': { label: '已失败', tone: 'bad' },
  'person/offline': { label: '执行者离线', tone: 'bad' },
  working: { label: '进行中', tone: 'brand' },
  idle: { label: '等待触发', tone: 'mono' },
  queued: { label: '排队中', tone: 'mono' },
  done: { label: '已完成', tone: 'ok' },
  cancelled: { label: '已取消', tone: 'mono' },
};

export function stageMeta(stage) {
  return STAGE_META[stage] || { label: stage || '未知', tone: 'mono' };
}

export function workFilterParams(filter = workState.filter, q = workState.q) {
  const params = new URLSearchParams();
  switch (filter) {
    case 'attention':
      params.set('stage', 'attention');
      break;
    case 'working':
      params.set('stage', 'working');
      break;
    case 'done':
      params.set('state', 'done');
      break;
    default:
      break;
  }
  if (q) params.set('q', q);
  return params;
}

let timer = null;

function emptyText() {
  if (workState.q) return '没有匹配的工作项';
  if (workState.filter === 'attention') return '没有需要你处理的工作';
  if (workState.filter === 'done') return '还没有已完成的工作';
  return '暂无工作项';
}

export async function refreshWork() {
  const table = $('workTable');
  if (!table) return;
  const tbody = table.querySelector('tbody');
  if (!tbody) return;
  tbody.innerHTML = `<tr class="skel"><td colspan="5"><i></i></td></tr>`;

  const params = workFilterParams();
  params.set('page', String(workState.page));
  params.set('page_size', String(workState.pageSize));
  try {
    const r = await api('/admin/tickets?' + params.toString());
    if (!r.ready) {
      tbody.innerHTML = `<tr><td colspan="5" class="empty-td"><div class="empty">${icon('clock')}<p>正在建立工作项索引…</p></div></td></tr>`;
      renderPager($('workPager'), { total: 0, page: 1, pageSize: workState.pageSize });
      return;
    }
    const items = r.items || [];
    if (!items.length) {
      tbody.innerHTML = `<tr><td colspan="5" class="empty-td"><div class="empty">${icon('ok')}<p>${esc(emptyText())}</p></div></td></tr>`;
      renderPager($('workPager'), { total: 0, page: 1, pageSize: workState.pageSize });
      await refreshWorkCounts();
      return;
    }
    tbody.innerHTML = items.map((t) => {
      const meta = stageMeta(t.effective_stage || t.stage);
      const offline = t.assignee_online === false ? `<span class="badge bad xs">${icon('alert', 'xs')}离线</span>` : '';
      return `<tr class="clickable" data-root="${esc(t.thread_root)}">
        <td>
          <div class="thread-cell">
            <code>#${t.id}</code>
            <small title="${esc(t.last_preview)}">${esc(t.last_preview || '（无消息）')}</small>
          </div>
        </td>
        <td>
          <div class="member-cell">
            <span>${esc(t.requester)}</span>
            <span class="muted">→</span>
            <span>${esc(t.assignee)}</span>
            ${offline}
          </div>
        </td>
        <td><span class="badge ${meta.tone}">${esc(meta.label)}</span></td>
        <td>
          <div class="time-cell">
            <span>${esc(ago(t.updated_at))}</span>
            <small class="muted">${fmtTime(t.updated_at)}</small>
          </div>
        </td>
        <td class="td-actions">
          <button class="btn btn-ghost btn-xs" data-view-work="${esc(t.thread_root)}">${icon('eye', 'xs')}查看线程</button>
        </td>
      </tr>`;
    }).join('');

    tbody.querySelectorAll('tr[data-root]').forEach((tr) => {
      tr.onclick = (e) => {
        if (e.target.closest('button')) return;
        viewThread(tr.dataset.root);
      };
    });
    tbody.querySelectorAll('button[data-view-work]').forEach((btn) => {
      btn.onclick = () => viewThread(btn.dataset.viewWork);
    });
    renderPager($('workPager'), {
      total: r.total || 0,
      page: r.page || 1,
      pageSize: r.page_size || workState.pageSize,
      onPage: (p) => { workState.page = p; refreshWork(); },
    });
    await refreshWorkCounts();
  } catch (e) {
    if (e.status === 401) return;
    tbody.innerHTML = `<tr><td colspan="5" class="empty-td"><div class="empty">${icon('alert')}<p>工作台加载失败：${esc(e.message)}</p></div></td></tr>`;
    toast('工作台加载失败：' + e.message, 'bad');
  }
}

async function refreshWorkCounts() {
  try {
    const [attention, open] = await Promise.all([
      api('/admin/attention?page_size=1'),
      api('/admin/tickets?state=open&page_size=1'),
    ]);
    const counts = attention.counts || {};
    const totalAttention = Object.entries(counts)
      .filter(([k]) => k.startsWith('person/'))
      .reduce((n, [, v]) => n + (v || 0), 0);
    setText('workAttention', totalAttention);
    setText('workWorking', counts.working || 0);
    setText('workOpen', open.total || 0);
    setText('workFcAttention', totalAttention);
    const navWork = $('navWork');
    if (navWork) {
      navWork.textContent = totalAttention > 0 ? totalAttention : '';
      navWork.hidden = totalAttention === 0;
    }
  } catch (_) {
    // Counts are decoration; the list above is the source of truth.
  }
}

function setText(id, value) {
  const el = $(id);
  if (el) el.textContent = value;
}

export function stopWork() {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
}

export function initWork() {
  const seg = $('workSeg');
  if (seg) {
    seg.querySelectorAll('button[data-v]').forEach((btn) => {
      btn.onclick = () => {
        seg.querySelectorAll('button').forEach((b) => b.classList.remove('on'));
        btn.classList.add('on');
        workState.filter = btn.dataset.v;
        workState.page = 1;
        refreshWork();
      };
    });
  }
  const search = $('workQ');
  if (search) {
    let debounce = null;
    search.oninput = () => {
      clearTimeout(debounce);
      debounce = setTimeout(() => {
        workState.q = search.value.trim();
        workState.page = 1;
        refreshWork();
      }, 300);
    };
  }
  const refresh = $('workRefresh');
  if (refresh) refresh.onclick = () => refreshWork();
  const auto = $('workAuto');
  if (auto) {
    auto.onchange = () => {
      stopWork();
      if (auto.checked) timer = setInterval(refreshWork, 10000);
    };
  }
}
