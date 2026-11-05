// Behavioural test for the 资源中心 console helpers.
//
//   node internal/web/ui/_resources-state.test.mjs
//
// Imports the real module so a syntax or import error also fails the lane.
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const mod = await import(pathToFileURL(path.join(here, 'js', 'resources.js')).href);
const { resURL, grantURL, grantSummary, grantRows, filterResources, NAME_RE, ALL_PEERS, RES_KINDS } = mod;

const assert = (cond, msg) => {
  if (!cond) {
    console.error('FAIL: ' + msg);
    process.exit(1);
  }
};

assert(resURL('secret', 'OPENAI_API_KEY') === '/admin/resources/secret/OPENAI_API_KEY', 'resource URL');
assert(resURL('secret', 'K', 'grants') === '/admin/resources/secret/K/grants', 'grants URL');
assert(grantURL('secret', 'K', ALL_PEERS) === '/admin/resources/secret/K/grants/*', 'wildcard revoke URL (* is path-safe)');
assert(grantURL('secret', 'K', 'a b') === '/admin/resources/secret/K/grants/a%20b', 'peer id encoded');

assert(grantSummary([]).text === '未授权', 'no grants');
assert(grantSummary(['alice', '*']).text === '全部助手', 'wildcard wins');
assert(grantSummary(['alice', 'bob']).text === '2 个助手', 'count');

const rows = grantRows([{ id: 'alice' }, { id: 'bob', online: true }], ['alice', '*']);
assert(rows[0].granted && !rows[0].viaWildcard, 'direct grant not shadowed');
assert(!rows[1].granted && rows[1].viaWildcard && rows[1].online, 'wildcard shadows the rest');
assert(grantRows([{ id: 'x' }], []).every((r) => !r.granted && !r.viaWildcard), 'nothing granted');

const list = [{ name: 'OPENAI_API_KEY', description: 'gpt' }, { name: 'GITHUB', description: 'repo bot' }];
assert(filterResources(list, 'bot').length === 1, 'filter by description');
assert(filterResources(list, 'openai').length === 1, 'filter case-insensitive');
assert(filterResources(list, '').length === 2, 'empty filter');

// Must match internal/resources nameRe.
for (const ok of ['K', 'OPENAI_API_KEY', 'a.b-c_1', 'x'.repeat(64)]) assert(NAME_RE.test(ok), 'name ok: ' + ok);
for (const bad of ['', '_x', '../x', 'a b', '${secret:x}', 'x'.repeat(65)]) assert(!NAME_RE.test(bad), 'name bad: ' + bad);

assert(RES_KINDS.map((k) => k.kind).join() === 'secret,mcp,skill,prompt', 'four kinds, fixed order');

console.log('ok resources-state');
