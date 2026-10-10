// Tokens tab module
import { api } from './api.js';
import { $, esc, icon, ago, fmtTime, avatar, renderPager, toast, confirmAction, setupDialog } from './utils.js';
import { refreshStats } from './auth.js';

export const tokenState = {
  page: 1,
  pageSize: 20,
  state: '', // '' | 'active' | 'revoked'
  q: '',
};

export async function refreshTokens() {
  const table = $('tokenTable');
  if (!table) return;
  const tbody = table.querySelector('tbody');
  if (!tbody) return;
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
            : `<button class="btn-icon danger" data-rev="${t.id}" data-peer="${esc(t.peer_id)}" data-tip="撤销此凭据" aria-label="撤销此凭据">${icon('ban')}</button>`}
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

export function initTokens() {
  const tokSeg = $('tokSeg');
  if (tokSeg) {
    tokSeg.querySelectorAll('button').forEach((b) => {
      b.onclick = () => {
        tokSeg.querySelectorAll('button').forEach((x) => x.classList.remove('on'));
        b.classList.add('on');
        tokenState.state = b.dataset.v;
        tokenState.page = 1;
        refreshTokens();
      };
    });
  }

  const tokQ = $('tokQ');
  let tokQTimer = null;
  if (tokQ) {
    tokQ.oninput = (e) => {
      clearTimeout(tokQTimer);
      tokQTimer = setTimeout(() => {
        tokenState.q = e.target.value.trim();
        tokenState.page = 1;
        refreshTokens();
      }, 260);
    };
  }

  const refreshTokensBtn = $('refreshTokens');
  if (refreshTokensBtn) refreshTokensBtn.onclick = refreshTokens;

  // Rotate Token Modal
  const rotDlg = $('rotateDlg');
  if (rotDlg) {
    setupDialog(rotDlg);
    const openRotate = $('openRotate');
    if (openRotate) {
      openRotate.onclick = () => {
        $('rotPeer').value = '';
        $('rotLabel').value = '';
        $('rotResult').hidden = true;
        $('rotBtn').disabled = false;
        rotDlg.showModal();
      };
    }

    const rotForm = $('rotForm');
    if (rotForm) {
      rotForm.onsubmit = async (e) => {
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
    }
  }
}
