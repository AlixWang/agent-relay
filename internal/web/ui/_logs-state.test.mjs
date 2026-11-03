// Behavioural test for the Agent log console helpers.
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const mod = await import(pathToFileURL(path.join(here, 'js', 'logs.js')).href);
const { logFilterParams, logLevelTone } = mod;

const assert = (cond, msg) => {
  if (!cond) {
    console.error('FAIL: ' + msg);
    process.exit(1);
  }
};

const params = logFilterParams({
  page: 2, pageSize: 50, peer: 'grace', level: 'error', source: 'relay-watch',
  event: 'worker.', q: 'worker timeout', range: '7d', thread: 'op/grace',
});
assert(params.get('page') === '2', 'page');
assert(params.get('peer') === 'grace', 'peer');
assert(params.get('level') === 'error', 'level');
assert(params.get('source') === 'relay-watch', 'source');
assert(params.get('event') === 'worker.', 'event');
assert(params.get('q') === 'worker timeout', 'q');
assert(params.get('range') === '7d', 'range');
assert(params.get('thread') === 'op/grace', 'thread');

assert(logLevelTone('error') === 'bad', 'error tone');
assert(logLevelTone('warn') === 'warn', 'warn tone');
assert(logLevelTone('info') === 'mono', 'info tone');
assert(logLevelTone('debug') === 'muted', 'debug tone');

console.log('PASS: agent log URL/level helpers');
