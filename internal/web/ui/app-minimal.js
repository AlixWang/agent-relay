// agent-relay minimal JS for HTMX + Templ
// Only theme toggle, toast notifications, and utility functions

/* ---------- Theme Toggle ---------- */
function initTheme() {
  const stored = localStorage.getItem('theme');
  const theme = stored || 'dark';
  document.documentElement.dataset.theme = theme;
  updateThemeIcon(theme);
}

function updateThemeIcon(theme) {
  const icon = document.querySelector('#themeToggle use');
  if (icon) {
    icon.setAttribute('href', theme === 'dark' ? '#i-sun' : '#i-moon');
  }
}

document.addEventListener('DOMContentLoaded', () => {
  initTheme();
  refreshSidebar();
  setInterval(refreshSidebar, 30000);

  const themeToggle = document.getElementById('themeToggle');
  if (themeToggle) {
    themeToggle.onclick = () => {
      const current = document.documentElement.dataset.theme;
      const next = current === 'dark' ? 'light' : 'dark';
      document.documentElement.dataset.theme = next;
      localStorage.setItem('theme', next);
      updateThemeIcon(next);
    };
  }

  // Mobile menu toggle
  const menuBtn = document.getElementById('menuBtn');
  const sbScrim = document.getElementById('sbScrim');
  if (menuBtn) {
    menuBtn.onclick = () => {
      document.getElementById('app').classList.toggle('nav-open');
    };
  }
  if (sbScrim) {
    sbScrim.onclick = () => {
      document.getElementById('app').classList.remove('nav-open');
    };
  }
});

/* ---------- Toast Notifications ---------- */
function toast(message, type = 'info', duration = 3200) {
  const container = document.getElementById('toasts');
  if (!container) return;

  const t = document.createElement('div');
  t.className = `toast toast-${type} ${type}`;
  const ic = type === 'ok' ? 'ok' : (type === 'bad' ? 'err' : (type === 'warn' ? 'alert' : 'info'));
  t.innerHTML = `<svg class="icon xs toast-icon"><use href="#i-${ic}"/></svg><span>${escapeHtml(message)}</span>`;

  container.appendChild(t);
  requestAnimationFrame(() => t.classList.add('show'));

  setTimeout(() => {
    t.classList.remove('show');
    setTimeout(() => t.remove(), 200);
  }, duration);
}

function escapeHtml(str) {
  const div = document.createElement('div');
  div.textContent = str;
  return div.innerHTML;
}

/* ---------- Sidebar telemetry (stats + inbox badge) ---------- */
async function refreshSidebar() {
  try {
    const r = await fetch('/admin/stats', { headers: { 'Content-Type': 'application/json' } });
    if (!r.ok) return;
    const s = await r.json();
    const set = (id, v) => {
      const el = document.getElementById(id);
      if (el) el.textContent = v;
    };
    set('sbVer', `v${s.protocol ?? '?'} · ${s.client_version || 'dev'}`);
    set('sbSeq', s.max_seq ?? '-');
    set('sbDb', `${Math.round((s.db_bytes || 0) / 1024)}K`);
    set('navOnline', s.peers_online ?? '');
    const inbox = document.getElementById('navInbox');
    if (inbox) {
      const n = s.inbox_unread || 0;
      inbox.textContent = n > 0 ? String(n) : '';
      inbox.hidden = n === 0;
    }
  } catch (e) {
    /* console telemetry is best-effort */
  }
}

/* ---------- Mention helper: click a chip to insert @id ---------- */
document.addEventListener('click', (evt) => {
  const chip = evt.target.closest('[data-mention]');
  if (!chip) return;
  const form = chip.closest('form');
  const box = form && form.querySelector('textarea[name="payload"]');
  if (!box) return;
  const mention = `@${chip.dataset.mention} `;
  const pos = box.selectionStart ?? box.value.length;
  box.value = box.value.slice(0, pos) + mention + box.value.slice(pos);
  box.focus();
  box.selectionStart = box.selectionEnd = pos + mention.length;
});

/* ---------- HTMX Configuration ---------- */
document.body.addEventListener('htmx:afterRequest', (evt) => {
  const xhr = evt.detail.xhr;
  if (xhr) {
    // Success toasts for mutations: the command center renders its own
    // contextual toast inside the swapped fragment, so only a plain "saved"
    // remains for everywhere else.
    if (evt.detail.successful && ['POST', 'PATCH', 'DELETE'].includes(evt.detail.verb)) {
      const elt = evt.detail.elt;
      if (!(elt && elt.closest && elt.closest('#page-command'))) {
        toast('操作成功', 'ok');
      }
    }
    // Show error toast
    if (!evt.detail.successful) {
      let msg = '操作失败';
      try {
        const body = JSON.parse(xhr.responseText);
        if (body.error) msg = body.error;
      } catch (e) {
        msg = xhr.statusText || msg;
      }
      toast(msg, 'bad');
    }
  }
});

// Auto-scroll new messages to bottom
document.body.addEventListener('htmx:afterSwap', (evt) => {
  if (evt.detail.target.id === 'convMessagesContainer') {
    evt.detail.target.scrollTop = evt.detail.target.scrollHeight;
  }
});
