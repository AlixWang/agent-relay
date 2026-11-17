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
  museVMText, museVMTitle,
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
assert(museWakeLabel({ wake_state: 'running' }) === '已唤醒', 'running label');
assert(museWakeLabel({}) === '未上线', 'no wake label');
assert(museWakeTone({ wake_state: 'recovered' }) === 'ok', 'recovered tone');
assert(museWakeTone({ wake_state: 'pending' }) === 'info', 'pending tone');

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

console.log('PASS: muse URL/state helpers');
