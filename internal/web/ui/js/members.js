// Members tab module
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, avatar, toast, confirmAction, renderPager } from './utils.js';
import { currentRoute } from './router.js';
import { serverPromptVersion, serverClientVersion, serverReceiverRev, refreshStats } from './auth.js';

export const peerState = {
  page: 1,
  pageSize: 20,
  filter: '',
  type: '',
  q: '',
  cache: [],
};

let peerRefreshTimer = null;

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
  const rev = p.client_rev || '';
  const when = p.client_updated_at ? fmtTime(p.client_updated_at) : '未知';
  if (rev && serverReceiverRev) {
    if (rev === serverReceiverRev) {
      return `<span class="badge ok mono" title="接收端与服务端同源码版本（${esc(v)}）">${esc(v)} <em class="muted">rev ${esc(rev)}</em></span>`;
    }
    return `<span class="badge warn mono" title="上报于 ${esc(when)} · rev ${esc(rev)} → 最新 rev ${esc(serverReceiverRev)}（${esc(serverClientVersion)}）">${esc(v)} → 新版</span>`;
  }
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

export async function refreshPeers() {
  const table = $('peerTable');
  if (!table) return;
  const tbody = table.querySelector('tbody');
  if (!tbody) return;
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

    const fc = r.facets || {};
    const fcAll = $('fcAll'); if (fcAll) fcAll.textContent = fc.all ?? '';
    const fcOnline = $('fcOnline'); if (fcOnline) fcOnline.textContent = fc.online ?? '';
    const fcActive = $('fcActive'); if (fcActive) fcActive.textContent = fc['status:active'] ?? '';
    const fcPending = $('fcPending'); if (fcPending) fcPending.textContent = (fc['status:pending'] || 0) + (fc['status:verifying'] || 0);
    const fcSuspended = $('fcSuspended'); if (fcSuspended) fcSuspended.textContent = fc['status:suspended'] ?? '';

    const typeSel = $('peerType');
    if (typeSel) {
      const curType = peerState.type;
      const typeCounts = r.types || {};
      typeSel.innerHTML = `<option value="">全部类型 (${fc.all || 0})</option>` +
        Object.keys(typeCounts).map((t) =>
          `<option value="${esc(t)}" ${t === curType ? 'selected' : ''}>${esc(t)} (${typeCounts[t]})</option>`
        ).join('');
    }

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

    tbody.querySelectorAll('.profile').forEach((el) => {
      el.onclick = () => el.classList.toggle('open');
    });

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

export function initMembers() {
  const peerSeg = $('peerSeg');
  if (peerSeg) {
    peerSeg.querySelectorAll('button').forEach((b) => {
      b.onclick = () => {
        peerSeg.querySelectorAll('button').forEach((x) => x.classList.remove('on'));
        b.classList.add('on');
        peerState.filter = b.dataset.v;
        peerState.page = 1;
        refreshPeers();
      };
    });
  }

  const peerType = $('peerType');
  if (peerType) {
    peerType.onchange = (e) => {
      peerState.type = e.target.value;
      peerState.page = 1;
      refreshPeers();
    };
  }

  const peerQ = $('peerQ');
  let peerQTimer = null;
  if (peerQ) {
    peerQ.oninput = (e) => {
      clearTimeout(peerQTimer);
      peerQTimer = setTimeout(() => {
        peerState.q = e.target.value.trim();
        peerState.page = 1;
        refreshPeers();
      }, 260);
    };
  }

  const refreshPeersBtn = $('refreshPeers');
  if (refreshPeersBtn) refreshPeersBtn.onclick = refreshPeers;

  const peerAuto = $('peerAuto');
  if (peerAuto) {
    peerAuto.onchange = (e) => {
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
  }
}
