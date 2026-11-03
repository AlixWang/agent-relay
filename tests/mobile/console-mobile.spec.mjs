// Browser-level regression checks for the two mobile failure classes that
// unit tests cannot see: cascade-order layout breakage and Markdown that never
// reached the thread inspector.
import { test, expect } from '@playwright/test';

const NOW = Math.floor(Date.now() / 1000);

const ROOM = {
  id: 'grp_ops',
  name: '运维小分队',
  note: '',
  created_at: NOW - 600,
  archived: false,
  count: 3,
  members: [
    { id: 'alice', status: 'active', online: true, prompt_version: 17, added_at: NOW - 600, start_seq: 0 },
    { id: 'bob', status: 'active', online: true, prompt_version: 17, added_at: NOW - 600, start_seq: 0 },
  ],
  last_seq: 3,
  last_sender: 'bob',
  last_kind: 'result',
  last_at: NOW - 20,
  last_preview: '- bob: 队列积压 0\n- bob: api-gateway 正常\n\n共同结论：bob 侧全部正常。',
};

const PEERS = [
  { id: 'alice', display_name: 'alice', agent_type: 'muse', online: true, status: 'active', prompt_version: 17 },
  { id: 'bob', display_name: 'bob', agent_type: 'claw', online: true, status: 'active', prompt_version: 17 },
];

const MESSAGES = [
  {
    seq: 1,
    id: 'sys-1',
    sender: 'system',
    recipient: 'grp_ops',
    kind: 'system',
    payload: '群聊 grp_ops 已创建（成员：alice、bob）。',
    created_at: NOW - 600,
    acked_by: [],
    acked_count: 0,
    approval_state: 'n/a',
  },
  {
    seq: 2,
    id: 'op-1',
    sender: 'operator',
    recipient: 'grp_ops',
    kind: 'task',
    payload: [
      '# 巡检任务：支付链路',
      '',
      '请 **alice** 和 **bob** 检查：',
      '',
      '| 项目 | 状态 |',
      '| --- | --- |',
      '| api-gateway | 正常 |',
      '| 队列 | 37 条 |',
      '',
      '```go',
      'fmt.Println("ok")',
      '```',
    ].join('\n'),
    created_at: NOW - 580,
    acked_by: ['alice', 'bob'],
    acked_count: 2,
    approval_state: 'n/a',
  },
  {
    seq: 3,
    id: 'bob-1',
    sender: 'bob',
    recipient: 'grp_ops',
    kind: 'result',
    payload: '- bob: 队列积压 0\n- bob: api-gateway 正常\n\n共同结论：bob 侧全部正常。',
    created_at: NOW - 20,
    acked_by: [],
    acked_count: 0,
    approval_state: 'n/a',
  },
];

const STATS = {
  ok: true,
  protocol: 1,
  client_version: 'mobile-test',
  max_seq: 3,
  db_bytes: 1024,
  peers_online: 2,
  peers_total: 2,
  threads_total: 1,
  tokens_active: 2,
  pending_approvals: 0,
  inbox_unread: 0,
  attention_tickets: 0,
  muse_needs_reauth: 0,
};

const json = (route, body, status = 200) => route.fulfill({
  status,
  contentType: 'application/json',
  body: JSON.stringify(body),
});

async function mockAdmin(page) {
  await page.route('**/admin/**', (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const method = route.request().method();

    if (path === '/admin/stats') return json(route, STATS);
    if (path === '/admin/rooms') return json(route, { rooms: [ROOM] });
    if (path === '/admin/inbox') return json(route, { unread: 0, items: [] });
    if (path === '/admin/peers') return json(route, { peers: PEERS, total: PEERS.length });
    if (path === '/admin/tickets') return json(route, { ready: true, items: [], total: 0, page: 1, page_size: 20 });
    if (path === '/admin/attention') return json(route, { counts: {}, items: [], total: 0 });

    if (path === '/admin/messages') {
      if (method === 'POST') return json(route, { ok: true, seq: 4, id: 'op-2', thread: 'grp_ops' });
      const thread = url.searchParams.get('thread');
      if (thread) return json(route, { items: MESSAGES, room: ROOM });
      if (url.searchParams.get('q')) return json(route, { items: MESSAGES, total: MESSAGES.length });
      return json(route, {
        threads: [{
          root_id: 'grp_ops',
          participants: ['operator', 'alice', 'bob'],
          count: MESSAGES.length,
          since_reset: MESSAGES.length,
          held: 0,
          fused: false,
          last_at: NOW - 20,
          last_preview: ROOM.last_preview,
        }],
        total: 1,
        fuse_max_messages: 200,
      });
    }

    return json(route, { ok: true, items: [], threads: [], peers: [], rooms: [] });
  });
}

const viewports = [
  { name: 'iphone-se', width: 320, height: 568 },
  { name: 'iphone-12', width: 390, height: 844 },
];

for (const viewport of viewports) {
  test.describe(`${viewport.name} (${viewport.width}px)`, () => {
    test.use({
      viewport: { width: viewport.width, height: viewport.height },
      deviceScaleFactor: 2,
      isMobile: true,
      hasTouch: true,
      locale: 'zh-CN',
    });

    test('command stream has no crushed headers or page overflow', async ({ page }) => {
      await mockAdmin(page);
      await page.goto('/#command?room=grp_ops');
      await expect(page.locator('.cmd-msg.msg-peer').first()).toBeVisible();
      await expect(page.locator('.cmd-msg.msg-self .rich-text .msg-table')).toBeVisible();
      await expect(page.locator('.cmd-msg.msg-self .rich-text .msg-code')).toBeVisible();

      const metrics = await page.evaluate(() => {
        const body = document.body;
        const author = document.querySelector('.cmd-msg.msg-peer .cmd-msg-author b');
        const title = document.querySelector('.cmd-chat-title h2');
        const header = document.querySelector('.cmd-chat-header');
        const stream = document.querySelector('.cmd-stream');
        const textarea = document.querySelector('#cmdComposerTextarea');
        const nameRect = author.getBoundingClientRect();
        return {
          viewportWidth: document.documentElement.clientWidth,
          docWidth: document.documentElement.scrollWidth,
          nameWidth: nameRect.width,
          nameHeight: nameRect.height,
          titleWidth: title.getBoundingClientRect().width,
          headerBottom: header.getBoundingClientRect().bottom,
          streamTop: stream.getBoundingClientRect().top,
          inputFontSize: parseFloat(getComputedStyle(textarea).fontSize),
          touchAction: getComputedStyle(body).touchAction,
          viewportMeta: document.querySelector('meta[name="viewport"]').getAttribute('content'),
        };
      });

      expect(metrics.docWidth).toBeLessThanOrEqual(metrics.viewportWidth + 1);
      expect(metrics.nameWidth).toBeGreaterThan(10);
      expect(metrics.nameHeight).toBeLessThan(30);
      expect(metrics.titleWidth).toBeGreaterThan(100);
      expect(metrics.headerBottom).toBeLessThanOrEqual(metrics.streamTop + 1);
      expect(metrics.inputFontSize).toBeGreaterThanOrEqual(16);
      expect(metrics.touchAction).toContain('pan-x');
      expect(metrics.touchAction).toContain('pan-y');
      expect(metrics.viewportMeta).toContain('maximum-scale=1');
      expect(metrics.viewportMeta).toContain('user-scalable=no');
    });

    test('thread drawer renders Markdown and keeps its menu on screen', async ({ page }) => {
      await mockAdmin(page);
      await page.goto('/#threads');
      await expect(page.locator('#threadTable tbody tr[data-root="grp_ops"]')).toBeVisible();
      await page.click('tr[data-root="grp_ops"]');
      await expect(page.locator('#threadDrawer[open]')).toBeVisible();

      const drawer = page.locator('#threadDrawer');
      await expect(drawer.locator('.rich-text h3')).toContainText('巡检任务');
      await expect(drawer.locator('.rich-text strong').first()).toBeVisible();
      await expect(drawer.locator('.rich-text .msg-table')).toBeVisible();
      await expect(drawer.locator('.rich-text .msg-code')).toBeVisible();

      const indent = await page.evaluate(() => {
        const msg = document.querySelector('#threadDrawer .msg');
        const body = msg.querySelector('.msg-body');
        return body.getBoundingClientRect().left - msg.getBoundingClientRect().left;
      });
      expect(indent).toBeLessThan(20);

      await page.click('#copySummary');
      await page.waitForTimeout(200);
      const menu = await page.locator('#copyDropdown .menu').evaluate((el) => {
        const r = el.getBoundingClientRect();
        return { left: r.left, right: r.right };
      });
      expect(menu.left).toBeGreaterThanOrEqual(0);
      expect(menu.right).toBeLessThanOrEqual(viewport.width);
    });

    test('empty work table stays inside the panel', async ({ page }) => {
      await mockAdmin(page);
      await page.goto('/#work');
      await expect(page.locator('#workTable td.empty-td')).toBeVisible();

      const metrics = await page.evaluate(() => {
        const table = document.querySelector('#workTable');
        const wrap = table.closest('.table-wrap');
        return {
          tableWidth: table.getBoundingClientRect().width,
          wrapWidth: wrap.clientWidth,
          emptyRight: table.querySelector('.empty').getBoundingClientRect().right,
          viewportWidth: document.documentElement.clientWidth,
        };
      });
      expect(metrics.tableWidth).toBeLessThanOrEqual(metrics.wrapWidth + 1);
      expect(metrics.emptyRight).toBeLessThanOrEqual(metrics.viewportWidth);
    });
  });
}
