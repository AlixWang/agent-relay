// Regression for the production #command incident: a 1036-message op/Lisa
// thread made the console render ~1000 Alpine rows (Markdown + a click.outside
// listener per row) and take 15s to load. These checks pin the two fixes:
//
//  1. the stream mounts only a ~50-row window and opens on the NEWEST page;
//  2. history/polling use before_seq / after_seq instead of re-fetching the
//     whole thread.
//
// The thread drawer gets the same treatment: newest 50 rows, then "加载更早".
import { test, expect } from '@playwright/test';

const NOW = Math.floor(Date.now() / 1000);

const makeMessages = (n) => Array.from({ length: n }, (_, i) => {
  const seq = i + 1;
  return {
    seq,
    id: 'lisa-' + seq,
    sender: seq % 7 === 0 ? 'operator' : 'lisa',
    recipient: seq % 7 === 0 ? 'grp_ops' : 'grp_ops',
    kind: 'status',
    payload: `status #${seq}: 接单→执行→超时重试（第 ${seq} 轮）`,
    created_at: NOW - (n - seq),
    acked_by: [],
    acked_count: 0,
    read: 0,
    total: 0,
    approval_state: 'n/a',
  };
});

// Mutable per-test store: a 50-row load and the 1000-row incident shape share
// the same app boot, which is what the timing comparison needs.
const state = {
  messages: makeMessages(1000),
  threadCalls: [],
};

const ROOM = {
  id: 'grp_ops',
  name: '运维小分队',
  note: '',
  created_at: NOW - 6000,
  archived: false,
  count: 1000,
  members: [
    { id: 'lisa', status: 'active', online: true, prompt_version: 17, added_at: NOW - 6000, start_seq: 0 },
    { id: 'bob', status: 'active', online: true, prompt_version: 17, added_at: NOW - 6000, start_seq: 0 },
  ],
  last_seq: 1000,
  last_sender: 'lisa',
  last_kind: 'status',
  last_at: NOW,
  last_preview: 'status #1000: 接单→执行→超时重试（第 1000 轮）',
};

const PEERS = [
  { id: 'lisa', display_name: 'Lisa', agent_type: 'muse', online: true, status: 'active', prompt_version: 17 },
  { id: 'bob', display_name: 'bob', agent_type: 'claw', online: true, status: 'active', prompt_version: 17 },
];

const STATS = {
  ok: true,
  protocol: 1,
  client_version: 'virtual-list-test',
  max_seq: 1000,
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

function threadPage(url) {
  const list = state.messages;
  const q = url.searchParams;
  const size = Number(q.get('page_size') || 50);
  let items;
  let hasMore = false;
  if (q.has('after_seq')) {
    const newer = list.filter((m) => m.seq > Number(q.get('after_seq')));
    items = newer.slice(0, size);
    hasMore = newer.length > items.length;
  } else if (q.has('before_seq')) {
    const older = list.filter((m) => m.seq < Number(q.get('before_seq')));
    items = older.slice(Math.max(0, older.length - size));
    hasMore = older.length > items.length;
  } else {
    const start = Math.max(0, list.length - size);
    items = list.slice(start);
    hasMore = start > 0;
  }
  return {
    ok: true,
    items,
    total: list.length,
    page_size: size,
    has_more: hasMore,
    oldest_seq: items.length ? items[0].seq : 0,
    newest_seq: items.length ? items[items.length - 1].seq : 0,
    room: { id: 'grp_ops', members: ['lisa', 'bob'], name: '运维小分队', archived: false },
  };
}

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
      if (method === 'POST') return json(route, { ok: true, seq: state.messages.length + 1, id: 'op-new', thread: 'grp_ops' });
      const thread = url.searchParams.get('thread');
      if (thread) {
        state.threadCalls.push(Object.fromEntries(url.searchParams));
        return json(route, threadPage(url));
      }
      if (url.searchParams.get('q')) return json(route, { items: state.messages, total: state.messages.length });
      return json(route, {
        threads: [{
          root_id: 'grp_ops',
          participants: ['operator', 'lisa'],
          count: state.messages.length,
          since_reset: state.messages.length,
          held: 0,
          fused: false,
          last_at: NOW,
          last_preview: ROOM.last_preview,
        }],
        total: 1,
        fuse_max_messages: 200,
      });
    }

    return json(route, { ok: true, items: [], threads: [], peers: [], rooms: [] });
  });
}

test.beforeEach(async ({ page }) => {
  state.messages = makeMessages(1000);
  state.threadCalls = [];
  await mockAdmin(page);
});

test('1000-message stream mounts a bounded window and loads like a short one', async ({ page }) => {
  // Same app boot, 50 rows: the baseline for "does not scale with history".
  state.messages = makeMessages(50);
  let t0 = Date.now();
  await page.goto('/#command?room=grp_ops');
  await expect(page.locator('#cmdStreamContainer .cmd-msg').first()).toBeVisible();
  const shortLoad = Date.now() - t0;
  const shortNodes = await page.locator('#cmdStreamContainer .cmd-msg').count();

  // The incident shape: 1000 rows. Only the visible window may be mounted and
  // the page must open on #1000, not on the oldest page.
  state.messages = makeMessages(1000);
  t0 = Date.now();
  await page.reload();
  await expect(page.locator('[data-vseq="1000"]')).toBeVisible();
  const longLoad = Date.now() - t0;
  const longNodes = await page.locator('#cmdStreamContainer .cmd-msg').count();

  expect(shortNodes).toBeLessThanOrEqual(100);
  expect(longNodes).toBeLessThanOrEqual(100);
  expect(longNodes).toBeGreaterThanOrEqual(20);
  expect(longNodes).toBeLessThanOrEqual(shortNodes + 10);
  // A full 1000-row render used to take ~15s in production; a virtual window
  // must stay in the same bracket as a 50-row thread (generous CI margin).
  expect(longLoad).toBeLessThan(Math.max(2500, shortLoad * 3));

  // Newest page only in the model: #900 is history, #1000 is what we open on.
  await expect(page.locator('[data-vseq="900"]')).toHaveCount(0);
  expect(await page.locator('#cmdStreamContainer .cmd-msg').last().getAttribute('data-vseq')).toBe('1000');
});

test('load earlier walks before_seq and keeps the DOM window bounded', async ({ page }) => {
  await page.goto('/#command?room=grp_ops');
  await expect(page.locator('[data-vseq="1000"]')).toBeVisible();

  // Scroll to the top of the stream: the sticky "加载更早消息" control is
  // there, and the initial page starts at #951.
  await page.evaluate(() => {
    const el = document.getElementById('cmdStreamContainer');
    const prev = el.style.scrollBehavior;
    el.style.scrollBehavior = 'auto';
    el.scrollTop = 0;
    el.dispatchEvent(new Event('scroll'));
    el.style.scrollBehavior = prev;
  });
  const older = page.locator('#cmdStreamContainer .cmd-load-earlier button');
  await expect(older).toBeVisible();
  await older.click();

  await expect.poll(() => state.threadCalls.some((q) => q.before_seq === '951' && q.page_size === '50'), { timeout: 5000 }).toBe(true);
  // The loaded history above the anchor is mounted (and the row just above
  // #951 is what the viewport lands on); #1000 remains the newest row.
  await expect(page.locator('[data-vseq="940"]')).toHaveCount(1);
  await expect(page.locator('[data-vseq="950"]')).toBeVisible();
  const nodes = await page.locator('#cmdStreamContainer .cmd-msg').count();
  expect(nodes).toBeLessThanOrEqual(100);

  // Scrolling back to the bottom slides the same bounded window back to the
  // newest rows; the model kept #1000 all along.
  await page.evaluate(() => {
    const el = document.getElementById('cmdStreamContainer');
    const prev = el.style.scrollBehavior;
    el.style.scrollBehavior = 'auto';
    el.scrollTop = el.scrollHeight;
    el.dispatchEvent(new Event('scroll'));
    el.style.scrollBehavior = prev;
  });
  await expect(page.locator('[data-vseq="1000"]')).toBeVisible();
  expect(await page.locator('#cmdStreamContainer .cmd-msg').count()).toBeLessThanOrEqual(100);
});

test('polling asks for after_seq=maxSeq and appends only new rows', async ({ page }) => {
  await page.goto('/#command?room=grp_ops');
  await expect(page.locator('[data-vseq="1000"]')).toBeVisible();

  // The first background tick must be a forward cursor, not a full re-fetch.
  await expect.poll(
    () => state.threadCalls.some((q) => q.after_seq === '1000' && q.page_size === '200'),
    { timeout: 8000 },
  ).toBe(true);

  // Route entry may load the pane more than once (deep-link select + session
  // refresh), but every one of those is a 50-row page, never the full thread.
  const initialLoads = state.threadCalls.filter((q) => !q.after_seq && !q.before_seq);
  expect(initialLoads.length).toBeGreaterThanOrEqual(1);
  for (const q of initialLoads) expect(q.page_size).toBe('50');

  // Two new status rows arrive; the poll after #1000 appends them and follows
  // at the bottom without remounting the whole thread.
  state.messages.push(
    { seq: 1001, id: 'lisa-1001', sender: 'lisa', recipient: 'grp_ops', kind: 'status', payload: 'status #1001', created_at: NOW + 1, acked_by: [], read: 0, total: 0, approval_state: 'n/a' },
    { seq: 1002, id: 'lisa-1002', sender: 'lisa', recipient: 'grp_ops', kind: 'status', payload: 'status #1002', created_at: NOW + 2, acked_by: [], read: 0, total: 0, approval_state: 'n/a' },
  );
  await expect(page.locator('[data-vseq="1002"]')).toBeVisible({ timeout: 8000 });
  const nodes = await page.locator('#cmdStreamContainer .cmd-msg').count();
  expect(nodes).toBeLessThanOrEqual(100);
});

test('thread drawer opens on the newest 50 rows and pages older history', async ({ page }) => {
  await page.goto('/#threads');
  await expect(page.locator('#threadTable tbody tr[data-root="grp_ops"]')).toBeVisible();
  await page.click('tr[data-root="grp_ops"]');
  await expect(page.locator('#threadDrawer[open]')).toBeVisible();

  const rows = page.locator('#threadDrawer .timeline .msg');
  await expect(rows).toHaveCount(50);
  await expect(page.locator('#drawerMeta')).toContainText('共 1000 条');
  await expect(page.locator('#threadDrawer [data-copy-msg="1000"]')).toHaveCount(1);
  await expect(page.locator('#threadDrawer [data-copy-msg="900"]')).toHaveCount(0);

  await page.click('#loadOlderThread');
  await expect(rows).toHaveCount(100);
  await expect(page.locator('#threadDrawer [data-copy-msg="900"]')).toHaveCount(0);
  await expect(page.locator('#threadDrawer [data-copy-msg="951"]')).toHaveCount(1);
  expect(state.threadCalls.some((q) => q.before_seq === '951' && q.page_size === '50')).toBe(true);
});
