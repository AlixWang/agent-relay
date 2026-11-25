// Router and navigation management for agent-relay console
import { $ } from './utils.js';

export const routeMeta = {
  work:    { title: '工作台', desc: '待我处理的工作、进行中任务与工作历史' },
  members: { title: '成员', desc: '注册助手状态、在线心跳与路由能力' },
  muse:    { title: 'Muse 保活', desc: 'Muse 账号授权、会话续期与沙盒唤醒' },
  threads: { title: '消息 & 线程', desc: '会话流转、熔断保护与安全审批' },
  command: { title: '指挥台', desc: '多助手群聊协同、定向指令调度与收件箱' },
  'muse-chat': { title: '直接对话', desc: '与已开通成员的 Muse agent 对话（消息对成员可见）' },
  prompts: { title: '邀请 & Prompt', desc: '助手入驻凭据签发与版本指令生成' },
  resources: { title: '资源中心', desc: '密钥、MCP、Skill 与 Prompt 的登记、授权与下发' },
  mcp:     { title: 'MCP 网关', desc: 'AdSense / Bing 工具、外部 Key、Peer 授权、二次批准与审计' },
  audit:   { title: '审计日志', desc: '管理事件、状态流转与安全审计追踪' },
  logs:    { title: 'Agent 日志', desc: '接收端、看门狗与唤醒链运行日志' },
  tokens:  { title: 'Token 管理', desc: '凭证轮换、前缀核验与实时吊销' },
  system:  { title: '策略 & 更新', desc: '运行策略参数、服务规格与自动升级' },
};

export let currentRoute = 'members';

const routeHandlers = new Map();
const routeLeaveHandlers = new Map();

export function onRoute(name, enterFn, leaveFn = null) {
  if (enterFn) routeHandlers.set(name, enterFn);
  if (leaveFn) routeLeaveHandlers.set(name, leaveFn);
}

export function setRoute(r) {
  // Support query in hash like #command?room=grp_ops
  let cleanRoute = r;
  if (cleanRoute.includes('?')) {
    cleanRoute = cleanRoute.split('?')[0];
  }
  if (!routeMeta[cleanRoute]) cleanRoute = 'members';

  // Call leave handler of previous route if any
  if (currentRoute && currentRoute !== cleanRoute) {
    const leave = routeLeaveHandlers.get(currentRoute);
    if (leave) leave();
  }

  currentRoute = cleanRoute;
  if (!location.hash.startsWith('#' + cleanRoute) || (r.includes('?') && location.hash !== '#' + r)) {
    location.hash = r;
  }

  const pal = $('cmdPalette');
  if (pal?.open) pal.close();

  document.querySelectorAll('.sb-link').forEach((l) => {
    l.classList.toggle('active', l.dataset.route === cleanRoute);
  });
  document.querySelectorAll('main > .page').forEach((sec) => {
    sec.hidden = sec.id !== `page-${cleanRoute}`;
  });

  const m = routeMeta[cleanRoute];
  const pt = $('pageTitle');
  if (pt) pt.textContent = m.title;
  const pd = $('pageDesc');
  if (pd) pd.textContent = m.desc;

  const app = $('app');
  if (app) app.classList.remove('nav-open');

  const main = $('main');
  if (main) main.classList.toggle('full-bleed', cleanRoute === 'command' || cleanRoute === 'muse-chat');

  // 指挥台 pins the shell to the viewport (.cmd-* scroll, not the window); every
  // other route keeps the document as the scroll container.
  document.body.classList.toggle('cmd-active', cleanRoute === 'command' || cleanRoute === 'muse-chat');

  const handler = routeHandlers.get(cleanRoute);
  if (handler) handler();
}

function applySidebarCollapsed(collapsed) {
  const app = $('app');
  if (app) app.classList.toggle('sb-collapsed', collapsed);
  if (collapsed) document.documentElement.dataset.sb = 'collapsed';
  else delete document.documentElement.dataset.sb;
  const btn = $('sbCollapseBtn');
  if (btn) {
    btn.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    btn.setAttribute('data-tip', collapsed ? '展开侧边栏' : '折叠侧边栏');
    btn.setAttribute('aria-label', collapsed ? '展开侧边栏' : '折叠侧边栏');
  }
  try { localStorage.setItem('ar-sb-collapsed', collapsed ? '1' : '0'); } catch {}
}

export function toggleSidebarCollapsed() {
  const cur = $('app')?.classList.contains('sb-collapsed') || document.documentElement.dataset.sb === 'collapsed';
  applySidebarCollapsed(!cur);
}

export function initNavigation() {
  let savedCollapsed = false;
  try { savedCollapsed = localStorage.getItem('ar-sb-collapsed') === '1'; } catch {}
  applySidebarCollapsed(savedCollapsed);

  document.querySelectorAll('.sb-link').forEach((l) => {
    const href = l.getAttribute('href') || '';
    if (!href.startsWith('#')) return;
    l.onclick = (e) => {
      e.preventDefault();
      setRoute(l.dataset.route || href.slice(1));
    };
  });
  window.addEventListener('hashchange', () => {
    const raw = location.hash.replace(/^#/, '');
    const clean = raw.split('?')[0];
    if (clean && clean !== currentRoute) setRoute(raw);
  });

  const sbCollapseBtn = $('sbCollapseBtn');
  if (sbCollapseBtn) sbCollapseBtn.onclick = toggleSidebarCollapsed;

  const onlinePill = $('onlinePill');
  if (onlinePill) {
    onlinePill.onclick = () => setRoute('members');
    onlinePill.onkeydown = (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        setRoute('members');
      }
    };
  }

  const menuBtn = $('menuBtn');
  const scrim = $('sbScrim');
  if (menuBtn) {
    menuBtn.onclick = () => $('app')?.classList.toggle('nav-open');
  }
  if (scrim) {
    scrim.onclick = () => $('app')?.classList.remove('nav-open');
  }
}
