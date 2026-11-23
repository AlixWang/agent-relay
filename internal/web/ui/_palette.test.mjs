// Unit tests for the global command palette (⌘K) helpers.
//
//   node internal/web/ui/_palette.test.mjs
import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const { buildPaletteItems, filterPaletteItems } = await import(pathToFileURL(path.join(here, 'js', 'palette.js')).href);

const routes = {
  work: { title: '工作台', desc: '待我处理的工作' },
  members: { title: '成员', desc: '注册助手状态' },
  command: { title: '指挥台', desc: '多助手群聊协同' },
  tokens: { title: 'Token 管理', desc: '凭证轮换' },
  'muse-chat': { title: '直接对话', desc: 'hidden subroute' },
};

const items = buildPaletteItems({
  routes,
  stats: {
    attention_tickets: 3,
    peers_online: 2,
    peers_total: 4,
    inbox_unread: 1,
  },
  peers: [
    { id: 'bob', agent_type: 'claw', online: false },
    { id: 'alice', agent_type: 'muse', online: true, display_name: 'Alice' },
  ],
  rooms: [
    { id: 'grp_ops', name: '运维群', archived: false, members: ['alice', 'bob'] },
    { id: 'grp_old', name: '旧群', archived: true, members: ['alice'] },
  ],
  theme: 'dark',
  collapsed: false,
});

assert.ok(!items.some((x) => x.route === 'muse-chat'), 'muse-chat subroute should not clutter root nav');
assert.ok(!items.some((x) => x.roomId === 'grp_old'), 'archived rooms should not appear in quick jump');
assert.equal(items.find((x) => x.route === 'work')?.badge?.text, '3 待处理');
assert.equal(items.find((x) => x.route === 'members')?.badge?.text, '2/4 在线');

const peerItems = items.filter((x) => x.kind === 'peer');
assert.equal(peerItems[0].peerId, 'alice', 'online peers must sort before offline peers');

// Empty query caps long lists cleanly
const capped = filterPaletteItems(buildPaletteItems({
  routes,
  peers: Array.from({ length: 20 }, (_, i) => ({ id: `p${i}`, online: i < 3 })),
  rooms: Array.from({ length: 12 }, (_, i) => ({ id: `r${i}`, archived: false, members: [] })),
}), '');
assert.equal(capped.filter((x) => x.kind === 'room').length, 5);
assert.equal(capped.filter((x) => x.kind === 'peer').length, 6);

// Query matching across Chinese title, route key, peer ID, and actions
assert.equal(filterPaletteItems(items, 'tok')[0].route, 'tokens');
assert.equal(filterPaletteItems(items, 'alice')[0].peerId, 'alice');
assert.equal(filterPaletteItems(items, '运维')[0].roomId, 'grp_ops');
assert.ok(filterPaletteItems(items, '浅色').some((x) => x.action === 'toggle-theme'));

console.log('palette helpers: all checks passed');
