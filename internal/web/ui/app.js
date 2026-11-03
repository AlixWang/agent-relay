// ESM entrypoint with Alpine.js

import Alpine from '/vendor/alpine.esm.js';
import { initTheme } from './js/utils.js';
import { initNavigation, setRoute, onRoute } from './js/router.js';
import { initAuth, refreshStats, showLogin } from './js/auth.js';
import { initWork, refreshWork, stopWork } from './js/work.js';
import { initMembers, refreshPeers } from './js/members.js';
import { initMuse, refreshMuse, stopMuse } from './js/muse.js';
import { initThreads, refreshThreads } from './js/threads.js';
import { initPrompts } from './js/prompts.js';
import { initAudit, refreshAudit } from './js/audit.js';
import { initLogs, enterLogs, stopLogs } from './js/logs.js';
import { initTokens, refreshTokens } from './js/tokens.js';
import { initSystem, refreshSystem } from './js/system.js';
import { commandApp, refreshCommand, stopCommand } from './js/command.js';

// The operations console is a fixed app shell: page-level pinch zoom shrinks
// the visual viewport and clips panes (the command stream, the composer).
// Android honours the viewport meta; iOS Safari ignores `user-scalable=no`
// since iOS 10, so block its two-finger gesture explicitly on touch devices.
// Desktop trackpad gestures and browser zoom shortcuts are untouched.
function initTouchGuards() {
  if (!window.matchMedia || !window.matchMedia('(pointer: coarse)').matches) return;
  for (const type of ['gesturestart', 'gesturechange', 'gestureend']) {
    document.addEventListener(type, (e) => e.preventDefault(), { passive: false });
  }
}

// Register Alpine.js components
Alpine.data('commandApp', commandApp);

// Wire route handlers
onRoute('work', refreshWork, stopWork);
onRoute('members', refreshPeers);
onRoute('muse', refreshMuse, stopMuse);
onRoute('threads', refreshThreads);
onRoute('command', refreshCommand, stopCommand);
onRoute('audit', refreshAudit);
onRoute('logs', enterLogs, stopLogs);
onRoute('tokens', refreshTokens);
onRoute('system', refreshSystem);

// Initialize application modules
initTouchGuards();
initTheme();
initNavigation();
initAuth();
initWork();
initMembers();
initMuse();
initThreads();
initPrompts();
initAudit();
initLogs();
initTokens();
initSystem();

// Boot Alpine.js reactivity engine
Alpine.start();

// Check authentication and restore initial route
(async () => {
  const loggedIn = await refreshStats();
  if (loggedIn) {
    const rawHash = location.hash.replace(/^#/, '') || 'members';
    setRoute(rawHash);
  } else {
    showLogin();
  }
})();
