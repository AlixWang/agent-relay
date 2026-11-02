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
const { museURL, museAuthTone, museAuthLabel, museExpiryText, museExpiryTone, museVMTone } = mod;

const assert = (cond, msg) => {
  if (!cond) {
    console.error('FAIL: ' + msg);
    process.exit(1);
  }
};

assert(museURL('ma_1') === '/admin/muse/accounts/ma_1', 'account URL');
assert(museURL('ma 1', 'wake') === '/admin/muse/accounts/ma%201/wake', 'encoded action URL');
assert(museURL('ma_1', 'keepalive') === '/admin/muse/accounts/ma_1/keepalive', 'keepalive URL');

assert(museAuthTone({ enabled: true, auth_state: 'ok', last_attempt_ok: true }) === 'ok', 'ok tone');
assert(museAuthTone({ enabled: true, auth_state: 'ok', last_attempt_ok: true, last_error: 'wake pending' }) === 'warn', 'warning tone');
assert(museAuthTone({ enabled: true, auth_state: 'ok', last_attempt_ok: false }) === 'warn', 'stale tone');
assert(museAuthTone({ enabled: true, auth_state: 'needs_reauth' }) === 'bad', 'reauth tone');
assert(museAuthTone({ enabled: false, auth_state: 'ok', last_attempt_ok: true }) === 'muted', 'disabled tone');
assert(museAuthLabel({ enabled: true, auth_state: 'ok', last_attempt_ok: true }) === '正常', 'ok label');
assert(museAuthLabel({ enabled: true, auth_state: 'ok', last_attempt_ok: true, last_error: 'wake pending' }) === '有告警', 'warning label');
assert(museAuthLabel({ enabled: true, auth_state: 'needs_reauth' }) === '待重新授权', 'reauth label');
assert(museAuthLabel({ enabled: false, auth_state: 'ok' }) === '已停用', 'disabled label');

const now = 1_900_000_000;
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

console.log('PASS: muse URL/state helpers');
