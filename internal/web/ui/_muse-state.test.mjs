// Behavioural test for the Muse console helpers.
//
//   node internal/web/ui/_muse-state.test.mjs
//
// The page's API URLs and state labels are easy to get subtly wrong (encoding,
// tone precedence, expiry boundaries). Import the real module so a syntax or
// import error also fails the console test lane.
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const mod = await import(pathToFileURL(path.join(here, 'js', 'muse.js')).href);
const {
  museURL, museAuthTone, museAuthLabel, museExpiryText, museExpiryTone, museVMTone,
  museSessionOK, museWakeUnconfirmed, museWakeTone, museWakeLabel,
  museVMText, museVMTitle, museChatHash, parseMuseChatHash,
  museChatCodeText, museChatErrorText, museChatBudget, museChatCountdown,
  museChatContextMatches, museChatJobHTML, museChatJobsHTML, museState,
  renderMuseChatMessages,
} = mod;

const assert = (cond, msg) => {
  if (!cond) {
    console.error('FAIL: ' + msg);
    process.exit(1);
  }
};

assert(museURL('ma_1') === '/admin/muse/accounts/ma_1', 'account URL');
assert(museURL('ma 1', 'wake') === '/admin/muse/accounts/ma%201/wake', 'encoded action URL');
assert(museURL('ma_1', 'keepalive') === '/admin/muse/accounts/ma_1/keepalive', 'keepalive URL');

const now = 1_900_000_000;
const wakeFailed = {
  enabled: true, auth_state: 'ok', last_attempt_ok: true,
  last_wake_at: now, last_wake_ok: false, last_wake_state: 'unconfirmed',
  last_wake_error: 'wake not confirmed (vm_state=UNKNOWN)',
};
assert(museAuthTone({ enabled: true, auth_state: 'ok', last_attempt_ok: true }) === 'ok', 'ok tone');
assert(museAuthTone({ enabled: true, auth_state: 'ok', last_attempt_ok: true, last_error: 'session HTTP 500' }) === 'warn', 'session warning tone');
assert(museAuthTone({ enabled: true, auth_state: 'ok', last_attempt_ok: false }) === 'warn', 'stale tone');
assert(museAuthTone({ enabled: true, auth_state: 'needs_reauth' }) === 'bad', 'reauth tone');
assert(museAuthTone({ enabled: false, auth_state: 'ok', last_attempt_ok: true }) === 'muted', 'disabled tone');
assert(museAuthTone(wakeFailed) === 'warn', 'wake failure tone');
assert(museAuthLabel({ enabled: true, auth_state: 'ok', last_attempt_ok: true }) === '正常', 'ok label');
assert(museAuthLabel({ enabled: true, auth_state: 'ok', last_attempt_ok: true, last_error: 'session HTTP 500' }) === '有告警', 'session warning label');
assert(museAuthLabel({ enabled: true, auth_state: 'needs_reauth' }) === '待重新授权', 'reauth label');
assert(museAuthLabel({ enabled: false, auth_state: 'ok' }) === '已停用', 'disabled label');
assert(museAuthLabel(wakeFailed) === '唤醒未确认', 'wake failure label');
assert(museAuthLabel({ ...wakeFailed, last_error: 'session HTTP 500' }) === '有告警', 'session error outranks wake error');

assert(museSessionOK({ enabled: true, auth_state: 'ok', last_attempt_ok: true }), 'healthy session');
assert(!museSessionOK({ enabled: true, auth_state: 'ok', last_attempt_ok: true, last_error: 'session HTTP 500' }), 'session error is unhealthy');
assert(!museSessionOK({ enabled: false, auth_state: 'ok', last_attempt_ok: true }), 'disabled is not healthy');
assert(museWakeUnconfirmed(wakeFailed), 'wake failure is unconfirmed');
assert(!museWakeUnconfirmed({ ...wakeFailed, wake_state: 'running', last_wake_error: '' }), 'confirmed wake clears stale failure');
assert(!museWakeUnconfirmed({ enabled: true, auth_state: 'ok', last_attempt_ok: true }), 'no wake event is not unconfirmed');
assert(museWakeTone(wakeFailed) === 'warn', 'wake failure tone');
assert(museWakeLabel(wakeFailed) === '唤醒未确认', 'wake failure label');
assert(museWakeLabel({ last_wake_at: now, last_wake_ok: true, wake_state: 'recovered' }) === '已上线', 'recovered label');
assert(museWakeLabel({ wake_state: 'pending' }) === '唤醒中', 'pending label');
assert(museWakeLabel({ wake_state: 'noise_confirmed' }) === 'Noise 已验证', 'noise label');
assert(museWakeLabel({ wake_state: 'running' }) === '已唤醒', 'running label (legacy)');
assert(museWakeLabel({}) === '未上线', 'no wake label');
assert(museWakeTone({ wake_state: 'recovered' }) === 'ok', 'recovered tone');
assert(museWakeTone({ wake_state: 'noise_confirmed' }) === 'ok', 'noise tone');
assert(museWakeTone({ wake_state: 'pending' }) === 'info', 'pending tone');
assert(!museWakeUnconfirmed({ wake_state: 'noise_confirmed', last_wake_ok: true }), 'noise-confirmed is not unconfirmed');
assert(museWakeUnconfirmed({ wake_state: 'noise_confirmed', last_wake_ok: false }), 'inconsistent noise state is unconfirmed');

assert(museExpiryText(0, now) === '未知', 'unknown expiry');
assert(museExpiryText(now - 1, now) === '已过期', 'expired expiry');
assert(museExpiryText(now + 600, now) === '10 分钟后', 'minutes expiry');
assert(museExpiryText(now + 3 * 3600, now) === '3 小时后', 'hours expiry');
assert(museExpiryText(now + 3 * 86400, now) === '3 天后', 'days expiry');
assert(museExpiryTone(now - 1, now) === 'bad', 'expired tone');
assert(museExpiryTone(now + 3600, now) === 'warn', 'soon tone');
assert(museExpiryTone(now + 7 * 86400, now) === 'ok', 'healthy tone');

assert(museVMTone('RUNNING') === 'ok', 'running vm');
assert(museVMTone('DISABLED') === 'muted', 'disabled vm');
assert(museVMTone('SUSPENDED') === 'warn', 'suspended vm');
assert(museVMTone('') === 'muted', 'unknown vm');
assert(museVMTone('UNKNOWN') === 'muted', 'explicit unknown vm');

assert(museVMText({ vm_state: 'RUNNING' }) === 'RUNNING', 'running vm text');
assert(museVMText({ vm_state: 'SUSPENDED' }) === 'SUSPENDED', 'suspended vm text');
assert(museVMText({ vm_state: 'UNKNOWN' }) === '未知（休眠/未分配）', 'unknown vm text');
assert(museVMText({ vm_state: '' }) === '未知（休眠/未分配）', 'missing vm text');
assert(museVMText({}) === '未知（休眠/未分配）', 'absent vm text');
const vmTip = museVMTitle({ vm_state: 'UNKNOWN', vm_state_at: now });
assert(vmTip.includes('原始值: UNKNOWN'), 'tooltip keeps raw vm value');
assert(vmTip.includes('确认于') && vmTip.includes('2030'), 'tooltip shows confirmation time');
assert(museVMTitle({ vm_state: '' }).includes('原始值: (空)'), 'tooltip marks empty raw value');
assert(museVMTitle({ vm_state: 'UNKNOWN' }).includes('尚无确认时间'), 'tooltip handles missing timestamp');
assert(museVMTitle({ vm_state: 'RUNNING' }).includes('仅展示，不参与唤醒判定'), 'vm state is display-only');

assert(museChatHash('alice') === '#muse-chat?peer=alice', 'main chat hash');
assert(museChatHash('alice', 's_1') === '#muse-chat?peer=alice&session=s_1', 'side chat hash');
assert(museChatHash('a b', 's 1') === '#muse-chat?peer=a+b&session=s+1', 'hash encodes spaces');
assert(parseMuseChatHash('#muse-chat?peer=alice').peer === 'alice', 'parse peer');
assert(parseMuseChatHash('#muse-chat?peer=alice&session=s_1').session === 's_1', 'parse session');
assert(parseMuseChatHash('#muse-chat').peer === '', 'parse empty');
assert(parseMuseChatHash('#muse').session === '', 'parse other route');

assert(museChatCodeText('muse_chat_in_progress').includes('不会排队'), 'in-progress copy');
assert(museChatErrorText({ error: { code: 'muse_delivery_unknown' } }).includes('不会自动重发'), 'delivery unknown copy');
assert(museChatErrorText(null) === '', 'no error copy');

assert(museChatBudget(180) === 180, 'explicit budget is kept');
assert(museChatBudget(10) === 30, 'budget floor');
assert(museChatBudget(999) === 300, 'budget ceiling');
assert(museChatBudget(200, 120) === 120, 'server max wins');
assert(museChatBudget('nope') === 300, 'bad budget falls back to the panel default');
assert(museChatCountdown(1000, 958) === '42s', 'countdown');
assert(museChatCountdown(1000, 1200) === '0s', 'countdown floor');
assert(museChatCountdown(0, 0) === '', 'no deadline');

const routeState = { chatLoadToken: 4, chatPeer: 'alice', chatSession: 's1' };
assert(museChatContextMatches({ loadToken: 4, peer: 'alice', session: 's1' }, routeState), 'chat context matches');
assert(!museChatContextMatches({ loadToken: 3, peer: 'alice', session: 's1' }, routeState), 'stale load token rejected');
assert(!museChatContextMatches({ loadToken: 4, peer: 'bob', session: 's1' }, routeState), 'stale peer rejected');
assert(!museChatContextMatches({ loadToken: 4, peer: 'alice', session: 's2' }, routeState), 'stale session rejected');

// --- job card markup -------------------------------------------------------
//
// The Muse chat stream is re-rendered on a ~1s poll tick. Anything that
// encodes the clock in the markup turns that tick into a full innerHTML
// replacement, which replays the card entry animation (the reported blink),
// kills text selection and jumps the scroll position. These asserts pin the
// two properties that keep the tick cheap: the markup is clock-free, and an
// unchanged thread renders byte-identical HTML.
const waitJob = {
  chat_id: 'mc_1', status: 'waiting', text: 'hi <there>', reply: null,
  partial_text: '', warnings: [], error: null, cancel: '',
  created_at: 1_900_000_000, updated_at: 1_900_000_010, deadline_at: 1_900_000_100,
};
const waitHTML = museChatJobHTML(waitJob);
assert(waitHTML === museChatJobHTML(waitJob), 'job card markup is deterministic');
assert(waitHTML.includes('data-deadline="1900000100"'), 'countdown carries its deadline');
assert(/<span class="mchat-countdown" data-deadline="1900000100"><\/span>/.test(waitHTML),
  'countdown span renders empty (seconds are patched, not rendered)');
assert(!waitHTML.includes('1900000100s'), 'seconds never enter the markup');
assert(waitHTML.includes('已投递，等待回复 · 剩余 '), 'waiting copy keeps the 剩余 label');
assert(waitHTML.includes('hi &lt;there&gt;'), 'operator text is escaped');
assert(!waitHTML.includes('is-new'), 'no animation class without a previous render');
assert(museChatJobHTML(waitJob, true).includes('cmd-msg msg-self is-new'), 'new card gets is-new');
assert(museChatJobHTML(waitJob, true).includes('cmd-msg msg-peer is-new'), 'new peer card gets is-new');

const draftHTML = museChatJobHTML({ ...waitJob, partial_text: 'still writing' });
assert(!draftHTML.includes('still writing'), 'draft text is not baked into the markup');
assert(draftHTML.includes('data-chat-draft="mc_1"'), 'draft alert is addressable for in-place text');
assert(draftHTML.includes('mchat-draft-text'), 'draft alert keeps its patch target');

const timeoutHTML = museChatJobHTML({ ...waitJob, status: 'timeout', deadline_at: 0, updated_at: 1_900_000_200 });
assert(timeoutHTML.includes('mchat-status-line warn'), 'timeout keeps the warn tone');
assert(timeoutHTML.includes('已投递，等待回复超时'), 'timeout copy');
assert(!timeoutHTML.includes('mchat-countdown'), 'no countdown once the wait ended');

const jobDone = { ...waitJob, chat_id: 'mc_2', status: 'replied', reply: { text: 'ok' }, deadline_at: 0 };
const liveJobs = [waitJob, jobDone];
const seenAll = new Set(['mc_1', 'mc_2']);
const stable = museChatJobsHTML(liveJobs, seenAll);
assert(stable === museChatJobsHTML(liveJobs, seenAll), 'unchanged thread renders identical HTML');
assert(!stable.includes('is-new'), 're-rendering a known thread animates nothing');
const fresh = museChatJobsHTML(liveJobs, new Set(['mc_1']));
assert(fresh.includes('is-new'), 'a job that appeared after the last render animates');
assert((fresh.match(/is-new/g) || []).length === 2, 'both cards of the new job animate');
assert(fresh.replace(/ is-new/g, '') === stable,
  'the animation class is the only difference between written markup and signature');
assert(!museChatJobsHTML(liveJobs, null).includes('is-new'), 'first paint animates nothing');

// --- poll tick against a stub DOM ------------------------------------------
//
// The renderer is driven with a minimal element: the contract is that a tick
// carrying the same jobs writes no innerHTML at all, and that the countdown
// and the streamed draft are patched as text instead. A regression here is
// exactly the reported "the page blinks on every second".
const stream = {
  dataset: {}, scrollTop: 0, clientHeight: 100, scrollHeight: 400, writes: 0,
  countdowns: new Map(), drafts: new Map(), _html: '',
};
Object.defineProperty(stream, 'innerHTML', {
  get() { return stream._html; },
  set(v) { stream.writes++; stream._html = v; },
});
stream.querySelectorAll = (sel) => {
  if (sel === '.mchat-countdown') {
    return [...stream._html.matchAll(/<span class="mchat-countdown" data-deadline="(\d+)"><\/span>/g)].map((m) => {
      if (!stream.countdowns.has(m[1])) {
        stream.countdowns.set(m[1], { dataset: { deadline: m[1] }, textContent: '' });
      }
      return stream.countdowns.get(m[1]);
    });
  }
  if (sel === '[data-chat-draft]') {
    return [...stream._html.matchAll(/<div class="alert alert-warn" data-chat-draft="([^"]+)"/g)].map((m) => {
      if (!stream.drafts.has(m[1])) {
        const target = { textContent: '' };
        stream.drafts.set(m[1], { dataset: { chatDraft: m[1] }, querySelector: () => target, target });
      }
      return stream.drafts.get(m[1]);
    });
  }
  return [];
};

const priorDocument = globalThis.document;
const realNow = Date.now;
globalThis.document = { getElementById: (id) => (id === 'mchatStream' ? stream : null) };

const baseSecs = 1_900_000_000;
Date.now = () => baseSecs * 1000;
museState.chatPeer = 'alice';
museState.accounts = [{ peer_id: 'alice', chat_enabled: true }];
museState.chatThread = { jobs: [waitJob] };
museState.chatSig = '';
museState.chatSeen = null;
renderMuseChatMessages();
assert(stream.writes === 1, 'first render writes the stream once');
const tick = stream.countdowns.get(String(waitJob.deadline_at));
assert(tick && tick.textContent === '100s', 'countdown is filled from the deadline');

Date.now = () => (baseSecs + 1) * 1000;
renderMuseChatMessages();
assert(stream.writes === 1, 'a tick with unchanged jobs does not rewrite the stream');
assert(tick.textContent === '99s', 'a tick patches the seconds in place');

museState.chatThread = { jobs: [{ ...waitJob, partial_text: 'still' }] };
renderMuseChatMessages();
const draftWrites = stream.writes;
assert(draftWrites === 2, 'a draft appearing rewrites the stream once');
assert(stream.drafts.get('mc_1').target.textContent === 'still', 'draft text is filled in');
museState.chatThread = { jobs: [{ ...waitJob, partial_text: 'still writing' }] };
renderMuseChatMessages();
assert(stream.writes === draftWrites, 'a growing draft does not rewrite the stream');
assert(stream.drafts.get('mc_1').target.textContent === 'still writing', 'a growing draft is patched in place');

museState.chatThread = { jobs: [{ ...waitJob, status: 'timeout', deadline_at: 0, updated_at: baseSecs + 2 }] };
renderMuseChatMessages();
assert(stream.writes === draftWrites + 1, 'a status change still rewrites the stream');

Date.now = realNow;
globalThis.document = priorDocument;

console.log('PASS: muse URL/state helpers');
