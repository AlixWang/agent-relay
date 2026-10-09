// Prompts and onboarding tab module
import { api } from './api.js';
import { $, toast, copyToClipboard, icon } from './utils.js';

export function initPrompts() {
  const genPrompt = $('genPrompt');
  if (genPrompt) {
    genPrompt.onclick = async () => {
      const typeInput = document.querySelector('input[name="pType"]:checked');
      const agentType = typeInput ? typeInput.value : 'muse';
      const peerId = $('pPeer').value.trim();

      genPrompt.disabled = true;
      genPrompt.classList.add('loading');
      try {
        const r = await api('/admin/prompts', {
          method: 'POST',
          body: JSON.stringify({
            agent_type: agentType,
            peer_id: peerId,
            create_invite: true,
          }),
        });
        $('promptOut').hidden = false;
        $('inviteCode').textContent = r.code;
        $('promptText').textContent = r.prompt;
        toast('邀请码与 Onboarding Prompt 已生成', 'ok');
      } catch (e) {
        toast('生成失败: ' + e.message, 'bad');
      } finally {
        genPrompt.disabled = false;
        genPrompt.classList.remove('loading');
      }
    };
  }

  const genReconf = $('genReconf');
  if (genReconf) {
    genReconf.onclick = async () => {
      const peer = $('rPeer').value.trim();
      if (!peer) {
        toast('请先选择或输入成员身份', 'warn');
        $('rPeer').focus();
        return;
      }
      genReconf.disabled = true;
      try {
        const r = await api('/admin/prompts', {
          method: 'POST',
          body: JSON.stringify({ agent_type: 'muse', peer_id: peer, reconfigure: true }),
        });
        $('reconfEmpty').hidden = true;
        $('reconfOut').hidden = false;
        $('reconfName').textContent = `${peer}-reconfigure.md`;
        $('reconfText').textContent =
          `# agent-relay 指令更新 · 身份: ${peer}\n# 沿用原 token，无需重新注册\n\n${r.prompt}`;
        toast(`已生成 ${peer} 的重配指令`, 'ok');
      } catch (e) {
        toast('生成重配失败: ' + e.message, 'bad');
      } finally {
        genReconf.disabled = false;
      }
    };
  }

  // Global click delegate for [data-copy] and [data-dl]
  document.addEventListener('click', async (e) => {
    if (!e.target.closest('details.dropdown')) {
      document.querySelectorAll('details.dropdown[open]').forEach((d) => { d.open = false; });
    }

    const copyBtn = e.target.closest('[data-copy]');
    if (copyBtn) {
      const selector = copyBtn.dataset.copy;
      const target = document.querySelector(selector);
      if (target) {
        const ok = await copyToClipboard(target.textContent || target.value || '');
        if (ok) {
          toast('已复制到剪贴板', 'ok', 1800);
          const origHtml = copyBtn.innerHTML;
          copyBtn.innerHTML = `${icon('check', 'xs')} 已复制`;
          setTimeout(() => { copyBtn.innerHTML = origHtml; }, 1600);
        } else {
          toast('复制失败，请重试', 'bad');
        }
      }
      return;
    }

    const dlBtn = e.target.closest('[data-dl]');
    if (dlBtn) {
      const target = document.querySelector(dlBtn.dataset.dl);
      const fname = dlBtn.dataset.name || 'download.txt';
      if (target) {
        const blob = new Blob([target.textContent || ''], { type: 'text/markdown;charset=utf-8' });
        const a = document.createElement('a');
        a.href = URL.createObjectURL(blob);
        a.download = fname;
        a.click();
        toast(`已下载 ${fname}`, 'ok', 1800);
      }
    }
  });
}
