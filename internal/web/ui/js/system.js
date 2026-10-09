// System policy and update tab module
import { api } from './api.js';
import { $, esc, icon, fmtTime, toast, confirmAction } from './utils.js';

let updJobTimer = null;

/* Update job progress.
   The job is tracked on the server (data_dir/update-jobs), because applying an
   update restarts the gateway: the panel keeps following it across that
   restart, and when the new version is serving, the page reloads itself. */
const UPD_LABEL = { queued: '排队中', running: '升级中', ok: '成功', rolled_back: '已回滚', failed: '失败' };
const UPD_TONE = { queued: 'warn', running: 'info', ok: 'ok', rolled_back: 'bad', failed: 'bad' };
let updSeenInFlight = new Set();   // job ids we watched run, this page view
let updJobFails = 0;

function updIsFinal(st) { return st && st !== 'queued' && st !== 'running'; }

function renderUpdJob(j) {
  const card = $('updJob');
  card.hidden = false;
  const state = $('updJobState');
  state.className = 'badge ' + (UPD_TONE[j.status] || 'info');
  state.textContent = UPD_LABEL[j.status] || j.status;
  $('updJobVer').textContent = j.version || '—';
  const meta = [];
  if (j.started_at) meta.push(fmtTime(j.started_at));
  if (j.elapsed_secs) meta.push((updIsFinal(j.status) ? '用时 ' : '已用 ') + j.elapsed_secs + 's');
  if (j.confirmed) meta.push('已生效');
  else if (j.status === 'ok') meta.push('等待服务重启');
  $('updJobMeta').textContent = meta.join(' · ');

  $('updSteps').innerHTML = (j.phases || []).map((p) => {
    const ic = p.status === 'done' ? 'ok' : (p.status === 'failed' ? 'err' : (p.status === 'active' ? 'rotate' : 'clock'));
    const detail = p.detail && p.status === 'failed' ? '：' + p.detail : '';
    return `<li class="${esc(p.status)}"${p.detail ? ` title="${esc(p.detail)}"` : ''}>${icon(ic, 'xs')}<span>${esc(p.name)}${esc(detail)}</span></li>`;
  }).join('');

  $('updJobToggle').hidden = false;

  const hint = $('updJobHint');
  if (j.hint) {
    hint.hidden = false;
    hint.innerHTML = `${icon('alert')}<span>${esc(j.hint)}</span>`;
  } else {
    hint.hidden = true;
  }

  // Follow the tail unless the operator scrolled up to read something.
  const log = $('updLog');
  const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 32;
  log.textContent = j.log || '（helper 还没有输出）';
  log.dataset.state = UPD_TONE[j.status] || 'info';
  if (atBottom) log.scrollTop = log.scrollHeight;
}

function updPollStop() {
  if (updJobTimer) { clearInterval(updJobTimer); updJobTimer = null; }
}

function updPollStart() {
  updPollStop();
  updJobFails = 0;
  updJobTimer = setInterval(updPoll, 2000);
}

async function updPoll() {
  let s;
  try {
    s = await api('/admin/update/status');
    updJobFails = 0;
  } catch (e) {
    // The update restarts the gateway: a few polls fail by design.
    updJobFails++;
    $('updJob').hidden = false;
    $('updJobState').className = 'badge warn';
    $('updJobState').textContent = '服务重启中';
    $('updJobMeta').textContent = `第 ${updJobFails} 次重试（等待新版本起来）…`;
    return;
  }
  if (!s.job) {
    // Between "helper restarted the service" and "the new process answers".
    $('updJob').hidden = false;
    $('updJobState').className = 'badge warn';
    $('updJobState').textContent = '服务重启中';
    $('updJobMeta').textContent = '等待新进程报告任务结果…';
    return;
  }
  renderUpdJob(s.job);
  if (!updIsFinal(s.job.status)) updSeenInFlight.add(s.job.id);
  if (!updIsFinal(s.job.status)) return;
  updPollStop();
  refreshSystem();
  if (s.job.status === 'ok') {
    toast(`已升级到 ${s.job.version}，正在刷新页面…`, 'ok');
    // Land the operator in the new version instead of asking for a manual
    // refresh (assets are versioned by the release tag).
    if (updSeenInFlight.has(s.job.id)) setTimeout(() => location.reload(), 1600);
  } else {
    toast(`升级未完成: ${s.job.status}`, 'bad');
  }
}

export async function refreshSystem() {
  // 1. Stats and Info
  try {
    const s = await api('/admin/stats');
    const sysInfo = $('sysInfo');
    if (sysInfo) {
      sysInfo.innerHTML = `
        <dt>${icon('hash', 'xs')} 最大消息序号 (max_seq)</dt><dd>${s.max_seq ?? '-'}</dd>
        <dt>${icon('db', 'xs')} SQLite 数据库体积</dt><dd>${((s.db_bytes || 0) / 1024).toFixed(0)} KiB</dd>
        <dt>${icon('package', 'xs')} 协议版本 (protocol)</dt><dd>v${s.protocol ?? '-'}</dd>
        <dt>${icon('terminal', 'xs')} 最低兼容客户端 (min_client)</dt><dd>v${s.min_client ?? '-'}</dd>
        <dt>${icon('wrench', 'xs')} 服务端指令版本</dt><dd>v${s.prompt_version ?? '-'}</dd>
        <dt>${icon('server', 'xs')} 官方客户端 Release</dt><dd>${esc(s.client_version || 'dev')}</dd>
        <dt>${icon('package', 'xs')} 接收端源码版本 (receiver_rev)</dt><dd>${esc(s.receiver_rev || 'dev（不催更新）')}</dd>
      `;
    }
  } catch {}

  // 2. Retention & Guard Policy
  try {
    const c = await api('/admin/config');
    const r = c.retention || {};
    const g = c.guard || {};
    const p = c.presence || {};
    const sysPolicy = $('sysPolicy');
    if (sysPolicy) {
      sysPolicy.innerHTML = `
        <dt>${icon('timer', 'xs')} 消息保留时长 (TTL)</dt><dd>${r.message_ttl_days} 天</dd>
        <dt>${icon('trash', 'xs')} 离线成员剪枝</dt><dd>${r.peer_prune_after_days} 天</dd>
        <dt>${icon('scroll', 'xs')} 审计日志保留</dt><dd>${r.audit_retention_days} 天</dd>
        <dt>${icon('alert', 'xs')} 会话熔断阈值</dt><dd>${g.fuse_max_messages} 条 / ${Math.floor((g.fuse_max_age_secs || 0) / 3600)} 小时</dd>
        <dt>${icon('activity', 'xs')} 发送限流速率</dt><dd>${g.rate_per_minute} 次 / 分钟</dd>
        <dt>${icon('radio', 'xs')} 在线判定超时</dt><dd>${p.online_timeout_secs} 秒</dd>
        <dt>${icon('shield', 'xs')} 冒烟验证超时</dt><dd>${p.verify_timeout_secs} 秒</dd>
      `;
    }
  } catch (err) {
    const sysPolicy = $('sysPolicy');
    if (sysPolicy) sysPolicy.innerHTML = `<p class="muted xs">配置读取失败：${esc(err.message)}</p>`;
  }

  // 3. Update Status
  try {
    const s = await api('/admin/update/status');
    const c = s.current || {};
    const updCurrent = $('updCurrent');
    if (updCurrent) {
      updCurrent.innerHTML = `当前版本: <b>${esc(c.tag || c.version || '?')}</b> · protocol <b>${c.protocol ?? '?'}</b> · min_client <b>${c.min_client ?? '?'}</b> · 部署模式: <span class="badge xs">${esc(s.mode || '?')}</span>`;
    }

    const updDockerCard = $('updDockerCard');
    const updApply = $('updApply');
    if (s.mode === 'docker') {
      if (updDockerCard) updDockerCard.hidden = false;
      if (updApply) updApply.disabled = true;
      const updDockerCmd = $('updDockerCmd');
      if (updDockerCmd) {
        updDockerCmd.textContent =
          `# 拉取目标版本镜像并重建容器（数据卷与配置保持不动）：\n` +
          `docker pull ghcr.io/alixwang/agent-relay:<目标版本> && \\\n` +
          `docker rm -f agent-relay && \\\n` +
          `docker run -d --name agent-relay --restart unless-stopped \\\n` +
          `  -v /var/lib/agent-relay:/var/lib/agent-relay \\\n` +
          `  -v /etc/agent-relay/config.toml:/etc/agent-relay/config.toml:ro \\\n` +
          `  ghcr.io/alixwang/agent-relay:<目标版本>`;
      }
    } else if (s.mode !== 'systemd') {
      if (updCurrent) updCurrent.innerHTML += ` <span class="muted xs">（非 systemd 环境，Web 自动更新已禁用）</span>`;
      if (updApply) updApply.disabled = true;
    }
    if (s.job) {
      renderUpdJob(s.job);
      if (!updIsFinal(s.job.status)) {
        updSeenInFlight.add(s.job.id);
        updPollStart();
      }
    } else if (!updJobTimer) {
      $('updJob').hidden = true;
    }
  } catch (e) {
    const updCurrent = $('updCurrent');
    if (updCurrent) updCurrent.textContent = '读取更新状态失败: ' + e.message;
  }
}

export function initSystem() {
  $('updJobToggle').onclick = () => {
    const log = $('updLog');
    log.hidden = !log.hidden;
  };

  const updCheck = $('updCheck');
  if (updCheck) {
    updCheck.onclick = async () => {
      const v = $('updVer').value.trim();
      const updApply = $('updApply');
      if (updApply) updApply.disabled = true;
      const updWarn = $('updWarn');
      if (updWarn) updWarn.innerHTML = `<p class="muted xs">正在检查${v ? '指定版本 ' + esc(v) : ' GitHub 最新 Release'}…</p>`;
      try {
        const r = await api('/admin/update/check', { method: 'POST', body: JSON.stringify({ version: v }) });
        const ver = r.version;
        $('updVer').value = ver;
        const changed = r.protocol_change || r.min_client_change;
        const updAckRow = $('updAckRow');
        if (r.unknown) {
          if (updWarn) updWarn.innerHTML = `<div class="alert alert-warn">${icon('alert')}<span><b>${esc(ver)}</b> 元数据不可达：继续升级将被视为跨协议变更，必须勾选下方确认项。</span></div>`;
          if (updAckRow) updAckRow.hidden = false;
        } else if (changed) {
          if (updWarn) updWarn.innerHTML = `<div class="alert alert-warn">${icon('alert')}<span><b>${esc(ver)}</b> 包含协议重大变更 (protocol_change=${r.protocol_change}, min_client_change=${r.min_client_change})：现有助手可能需要重新运行 Onboarding Prompt，必须勾选确认。</span></div>`;
          if (updAckRow) updAckRow.hidden = false;
        } else {
          if (updWarn) updWarn.innerHTML = `<div class="alert alert-ok">${icon('ok')}<span><b>${esc(ver)}</b> 协议完全兼容，支持无缝热更新（包含自动备份、健康检查与失败回滚）。</span></div>`;
          if (updAckRow) updAckRow.hidden = true;
        }
        if (updApply) {
          updApply.disabled = false;
          updApply.dataset.version = ver;
        }
      } catch (e) {
        toast('检查更新失败: ' + e.message, 'bad');
        if (updWarn) updWarn.innerHTML = `<div class="alert alert-bad">${icon('err')}<span>检查失败：${esc(e.message)}</span></div>`;
      }
    };
  }

  const updApply = $('updApply');
  if (updApply) {
    updApply.onclick = async () => {
      const v = updApply.dataset.version;
      if (!v) return;
      const updAckRow = $('updAckRow');
      const updAck = $('updAck');
      if (updAckRow && !updAckRow.hidden && updAck && !updAck.checked) {
        toast('跨协议更新必须先勾选确认条款', 'warn');
        return;
      }
      const ok = await confirmAction({
        title: `应用版本更新到 ${v}？`,
        body: `服务将在后台下载二进制文件，验证 SHA256，备份当前运行文件并重启 systemd 单元。\n健康检查失败将自动回滚。`,
        danger: false,
        okText: '立即更新',
      });
      if (!ok) return;

      try {
        const r = await api('/admin/update/apply', {
          method: 'POST',
          body: JSON.stringify({ version: v, acknowledge_protocol_change: updAck ? updAck.checked : false }),
        });
        updSeenInFlight.add(r.job_id);
        $('updJob').hidden = false;
        $('updJobState').className = 'badge warn';
        $('updJobState').textContent = '排队中';
        $('updJobVer').textContent = v;
        $('updJobMeta').textContent = `任务 ${r.job_id}`;
        $('updLog').textContent = '触发已写入，等待 root timer（最长 30 秒）取件…';
        updPollStart();
        updPoll();
      } catch (e) {
        toast('升级触发失败: ' + e.message, 'bad');
      }
    };
  }
}

export { renderUpdJob, updPoll, updPollStart, updPollStop, updIsFinal };
