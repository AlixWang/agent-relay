// Utilities and UI helpers for agent-relay console

export const msgPayloadMap = new Map();

export async function copyToClipboard(text) {
  if (text == null) return false;
  const str = String(text);
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(str);
      return true;
    } catch (_) {}
  }
  try {
    const ta = document.createElement('textarea');
    ta.value = str;
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    ta.style.top = '-9999px';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    if (ok) return true;
  } catch (_) {}
  return false;
}

export const $ = (id) => document.getElementById(id);

export const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
}[c]));

export const icon = (name, cls = '') =>
  `<svg class="icon ${cls}"><use href="#i-${name}"/></svg>`;

export const ago = (ts) => {
  if (!ts) return '从未';
  const d = Math.floor(Date.now() / 1000 - ts);
  if (d < 5) return '刚刚';
  if (d < 60) return d + 's 前';
  if (d < 3600) return Math.floor(d / 60) + 'm 前';
  if (d < 86400) return Math.floor(d / 3600) + 'h 前';
  return Math.floor(d / 86400) + 'd 前';
};

export const fmtTime = (ts) => {
  if (!ts) return '-';
  const d = new Date(ts * 1000);
  const pad = (n) => String(n).padStart(2, '0');
  const Y = d.getFullYear();
  const M = pad(d.getMonth() + 1);
  const D = pad(d.getDate());
  const h = pad(d.getHours());
  const m = pad(d.getMinutes());
  const s = pad(d.getSeconds());
  return `${Y}/${M}/${D} ${h}:${m}:${s}`;
};

export const hashHue = (str) => {
  let h = 0;
  for (let i = 0; i < str.length; i++) {
    h = ((h << 5) - h + str.charCodeAt(i)) & 0xffffffff;
  }
  return Math.abs(h) % 360;
};

export const avatar = (id, cls = '') => {
  const safe = String(id || '?');
  const hue = hashHue(safe);
  const initials = safe.slice(0, 2).toUpperCase();
  return `<div class="avatar ${cls}" style="--h: ${hue}">${esc(initials)}</div>`;
};

export function toast(message, type = 'info', duration = 3200) {
  const container = $('toasts');
  if (!container) return;
  const t = document.createElement('div');
  t.className = `toast toast-${type} ${type}`;
  const ic = type === 'ok' ? 'ok' : (type === 'bad' ? 'err' : (type === 'warn' ? 'alert' : 'info'));
  t.innerHTML = `${icon(ic)}<p>${esc(message)}</p><button aria-label="关闭">${icon('x', 'xs')}</button>`;
  const close = () => {
    t.classList.add('leaving');
    setTimeout(() => t.remove(), 200);
  };
  t.querySelector('button').onclick = close;
  container.appendChild(t);
  if (duration > 0) {
    setTimeout(close, duration);
  }
}

export function setupDialog(dlg) {
  if (!dlg) return;
  if (!('closedBy' in HTMLDialogElement.prototype)) {
    dlg.addEventListener('click', (e) => {
      if (e.target !== dlg) return;
      const rect = dlg.getBoundingClientRect();
      const isContent = (
        rect.top <= e.clientY && e.clientY <= rect.top + rect.height &&
        rect.left <= e.clientX && e.clientX <= rect.left + rect.width
      );
      if (!isContent) dlg.close();
    });
  }
  dlg.querySelectorAll('[data-close]').forEach((btn) => {
    btn.onclick = () => dlg.close();
  });
}

export function confirmAction({ title = '确认操作', body = '此操作不可撤销，是否继续？', danger = true, okText = '确认' }) {
  const dlg = $('confirmDlg');
  if (!dlg) return Promise.resolve(confirm(body));
  $('cfTitle').textContent = title;
  $('cfBody').textContent = body;
  const okBtn = $('cfOk');
  okBtn.textContent = okText;
  okBtn.className = danger ? 'btn btn-danger' : 'btn btn-primary';
  const ic = $('cfIc');
  ic.className = danger ? 'card-ic bad' : 'card-ic warn';
  ic.innerHTML = icon(danger ? 'trash' : 'alert');

  return new Promise((resolve) => {
    const handler = () => {
      dlg.removeEventListener('close', handler);
      resolve(dlg.returnValue === 'ok');
    };
    dlg.addEventListener('close', handler);
    dlg.showModal();
  });
}

export function initTheme() {
  const btn = $('themeBtn');
  const getTheme = () => document.documentElement.dataset.theme || 'dark';
  const applyTheme = (t) => {
    document.documentElement.dataset.theme = t;
    try { localStorage.setItem('ar-theme', t); } catch {}
    if (btn) {
      btn.innerHTML = icon(t === 'dark' ? 'sun' : 'moon');
      btn.setAttribute('data-tip', t === 'dark' ? '切换浅色主题' : '切换深色主题');
    }
  };
  applyTheme(getTheme());
  if (btn) {
    btn.onclick = () => applyTheme(getTheme() === 'dark' ? 'light' : 'dark');
  }
}

export function renderPager(targetEl, { total, page, pageSize, onPage, onPageSize }) {
  if (!targetEl) return;
  if (!total || total <= 0) {
    targetEl.innerHTML = '';
    return;
  }
  const totalPages = Math.ceil(total / pageSize) || 1;
  const start = (page - 1) * pageSize + 1;
  const end = Math.min(page * pageSize, total);

  const pages = [];
  if (totalPages <= 7) {
    for (let i = 1; i <= totalPages; i++) pages.push(i);
  } else {
    pages.push(1);
    if (page > 3) pages.push('...');
    const pStart = Math.max(2, page - 1);
    const pEnd = Math.min(totalPages - 1, page + 1);
    for (let i = pStart; i <= pEnd; i++) pages.push(i);
    if (page < totalPages - 2) pages.push('...');
    pages.push(totalPages);
  }

  targetEl.innerHTML = `
    <div class="pager-info">
      <span>第 <b>${start}</b> - <b>${end}</b> 条，共 <b>${total}</b> 条</span>
      <select class="pager-size" aria-label="每页条数">
        ${[10, 20, 50, 100].map((s) => `<option value="${s}" ${s === pageSize ? 'selected' : ''}>${s} 条/页</option>`).join('')}
      </select>
    </div>
    <div class="pager-nav">
      <button class="p-prev-all" ${page <= 1 ? 'disabled' : ''} title="第一页" aria-label="第一页">${icon('chevs-l')}</button>
      <button class="p-prev" ${page <= 1 ? 'disabled' : ''} title="上一页" aria-label="上一页">${icon('chev-l')}</button>
      ${pages.map((p) => {
        if (p === '...') return `<span class="gap">…</span>`;
        return `<button class="p-num ${p === page ? 'on' : ''}" data-p="${p}">${p}</button>`;
      }).join('')}
      <button class="p-next" ${page >= totalPages ? 'disabled' : ''} title="下一页" aria-label="下一页">${icon('chev-r')}</button>
      <button class="p-next-all" ${page >= totalPages ? 'disabled' : ''} title="最后页" aria-label="最后页">${icon('chevs-r')}</button>
    </div>
  `;

  targetEl.querySelector('.pager-size').onchange = (e) => {
    onPageSize?.(Number(e.target.value));
  };
  targetEl.querySelector('.p-prev-all').onclick = () => onPage?.(1);
  targetEl.querySelector('.p-prev').onclick = () => onPage?.(Math.max(1, page - 1));
  targetEl.querySelector('.p-next').onclick = () => onPage?.(Math.min(totalPages, page + 1));
  targetEl.querySelector('.p-next-all').onclick = () => onPage?.(totalPages);
  targetEl.querySelectorAll('.p-num').forEach((b) => {
    b.onclick = () => onPage?.(Number(b.dataset.p));
  });
}
