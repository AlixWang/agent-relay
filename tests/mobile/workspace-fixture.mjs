export const member = 'release-observer-east-asia';

export async function mockWorkspace(page, status = 'timeout') {
  const now = Math.floor(Date.now() / 1000);
  const summaries = ['确认周末巡检窗口的值班安排', '请核对发布清单中的依赖版本，并补充回退步骤', '复核数据归档结果', '等待成员提交本周的容量报告'];
  const stages = ['person/approval', 'person/review', 'person/blocked', 'person/answer'];
  const items = summaries.map((last_preview, index) => ({ id: 2041 + index, last_preview,
    stage: stages[index], thread_root: `work-${index}`, updated_at: now - 300 * (1 + index),
    requester: 'operator', assignee: index === 1 ? member : 'ops-team', assignee_online: true }));
  const latest = { chat_id: 'latest', status, text: '请再确认夜间值班窗口。', created_at: now - 300,
    updated_at: now - 120, user_seq: 12, deadline_at: now + 150 };
  const jobs = [{ chat_id: 'prior', status: 'replied', text: '汇总一下今天的巡检结果。',
    created_at: now - 600, updated_at: now - 590, reply: { text: '已完成服务和队列巡检，所有节点都已返回结果。\n\n网关响应正常，消息投递没有积压。值班窗口仍需确认，确认后会同步给执行成员。' } }, latest];
  const responses = {
    '/admin/stats': { protocol: 1, client_version: 'preview', peers_online: 2, peers_total: 3 },
    '/admin/tickets': { ready: true, items, total: items.length, page: 1, page_size: 20 },
    '/admin/attention': { counts: { 'person/approval': 4, working: 0 } },
    '/admin/muse/accounts': { accounts: [member, 'ops-team'].map(peer_id => ({ id: peer_id, peer_id, enabled: true, chat_enabled: true })), chat_status: { deploy_enabled: true, runtime_enabled: true } },
    '/admin/muse/chat/thread': { peer_id: member, sessions: ['night-shift'], jobs },
    '/admin/muse/chat/latest': latest,
  };
  await page.route('**/admin/**', route => route.fulfill({ json: responses[new URL(route.request().url()).pathname] || { items: [], peers: [], rooms: [] } }));
}
