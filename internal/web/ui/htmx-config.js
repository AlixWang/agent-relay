// HTMX configuration and extensions
document.addEventListener('DOMContentLoaded', () => {
  // Configure HTMX defaults
  htmx.config.defaultSwapStyle = 'innerHTML';
  htmx.config.defaultSwapDelay = 0;
  htmx.config.timeout = 30000; // 30 seconds
  htmx.config.historyCacheSize = 0; // Disable history cache

  // Auto-close modals on successful form submission
  document.body.addEventListener('htmx:afterRequest', (evt) => {
    if (evt.detail.successful && evt.detail.verb === 'POST') {
      const form = evt.detail.elt;
      const modal = form.closest('dialog');
      if (modal && modal.id === 'createConvDlg') {
        modal.remove();
      }
    }
  });

  // Handle drawer close button
  document.body.addEventListener('click', (evt) => {
    const closeBtn = evt.target.closest('[data-close]');
    if (closeBtn) {
      const drawer = closeBtn.closest('dialog');
      if (drawer) drawer.remove();
    }
  });
});
