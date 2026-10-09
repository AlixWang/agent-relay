// Audit log tab module
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, avatar, renderPager, toast } from './utils.js';

export const auditState = {
  page: 1,
  pageSize: 20,
  actor: '',
  action: '',
  q: '',
  cache: [],
};

export async function refreshAudit() {
  const table = $('auditTable');
  if (!table) return;
  const tbody = table.querySelector('tbody');
  if (!tbody) return;
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

    const aActor = $('aActor');
    if (aActor && Array.isArray(r.actors)) {
      const curActor = auditState.actor;
      aActor.innerHTML = `<option value="">全部 actor</option>` +
        r.actors.map((a) => `<option value="${esc(a)}" ${a === curActor ? 'selected' : ''}>${esc(a)}</option>`).join('');
    }
    const aAction = $('aAction');
    if (aAction && Array.isArray(r.actions)) {
      const curAction = auditState.action;
      aAction.innerHTML = `<option value="">全部 action</option>` +
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

export function initAudit() {
  const aActor = $('aActor');
  if (aActor) {
    aActor.onchange = (e) => {
      auditState.actor = e.target.value;
      auditState.page = 1;
      refreshAudit();
    };
  }

  const aAction = $('aAction');
  if (aAction) {
    aAction.onchange = (e) => {
      auditState.action = e.target.value;
      auditState.page = 1;
      refreshAudit();
    };
  }

  const aQ = $('aQ');
  let aQTimer = null;
  if (aQ) {
    aQ.oninput = (e) => {
      clearTimeout(aQTimer);
      aQTimer = setTimeout(() => {
        auditState.q = e.target.value.trim();
        auditState.page = 1;
        refreshAudit();
      }, 300);
    };
  }

  const refreshAuditBtn = $('refreshAudit');
  if (refreshAuditBtn) refreshAuditBtn.onclick = refreshAudit;

  const exportAudit = $('exportAudit');
  if (exportAudit) {
    exportAudit.onclick = () => {
      const blob = new Blob([JSON.stringify(auditState.cache, null, 2)], { type: 'application/json' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = `audit-${Date.now()}.json`;
      a.click();
      toast('已导出审计记录', 'ok');
    };
  }
}
