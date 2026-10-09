// Router and navigation management for agent-relay console
import { $ } from './utils.js';

export const routeMeta = {
  members: { title: '成员', desc: '注册助手状态、在线心跳与路由能力' },
  threads: { title: '消息 & 线程', desc: '会话流转、熔断保护与安全审批' },
  command: { title: '指挥台', desc: '多助手群聊协同、定向指令调度与收件箱' },
  prompts: { title: '邀请 & Prompt', desc: '助手入驻凭据签发与版本指令生成' },
  audit:   { title: '审计日志', desc: '管理事件、状态流转与安全审计追踪' },
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
  if (!location.hash.startsWith('#' + cleanRoute)) {
    location.hash = cleanRoute;
  }

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
  if (main) main.classList.toggle('full-bleed', cleanRoute === 'command');

  const handler = routeHandlers.get(cleanRoute);
  if (handler) handler();
}

export function initNavigation() {
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

  const menuBtn = $('menuBtn');
  const scrim = $('sbScrim');
  if (menuBtn) {
    menuBtn.onclick = () => $('app')?.classList.toggle('nav-open');
  }
  if (scrim) {
    scrim.onclick = () => $('app')?.classList.remove('nav-open');
  }
}
