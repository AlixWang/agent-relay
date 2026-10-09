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

/* ---------- HTMX Configuration ---------- */
document.body.addEventListener('htmx:afterRequest', (evt) => {
  const xhr = evt.detail.xhr;
  if (xhr) {
    // Show success toast for mutations
    if (evt.detail.successful && ['POST', 'PATCH', 'DELETE'].includes(evt.detail.verb)) {
      toast('操作成功', 'ok');
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
