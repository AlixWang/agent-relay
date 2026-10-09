// ESM entrypoint with Alpine.js

import Alpine from '/vendor/alpine.esm.js';
import { initTheme } from './js/utils.js';
import { initNavigation, setRoute, onRoute } from './js/router.js';
import { initAuth, refreshStats, showLogin } from './js/auth.js';
import { initMembers, refreshPeers } from './js/members.js';
import { initThreads, refreshThreads } from './js/threads.js';
import { initPrompts } from './js/prompts.js';
import { initAudit, refreshAudit } from './js/audit.js';
import { initTokens, refreshTokens } from './js/tokens.js';
import { initSystem, refreshSystem } from './js/system.js';
import { commandApp } from './js/command.js';

// Register Alpine.js components
Alpine.data('commandApp', commandApp);

// Wire route handlers
onRoute('members', refreshPeers);
onRoute('threads', refreshThreads);
onRoute('audit', refreshAudit);
onRoute('tokens', refreshTokens);
onRoute('system', refreshSystem);

// Initialize application modules
initTheme();
initNavigation();
initAuth();
initMembers();
initThreads();
initPrompts();
initAudit();
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
