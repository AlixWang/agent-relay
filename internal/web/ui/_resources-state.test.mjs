// Behavioural test for the 资源中心 console helpers.
//
//   node internal/web/ui/_resources-state.test.mjs
//
// Imports the real module so a syntax or import error also fails the lane.
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const mod = await import(pathToFileURL(path.join(here, 'js', 'resources.js')).href);
const { resURL, grantURL, grantSummary, grantRows, filterResources, NAME_RE, ALL_PEERS, RES_KINDS, kindLabel, secretRefsIn, buildPutBody, MCP_EXAMPLE, checkSkillFile, humanSize, MAX_SKILL_UPLOAD, RES_VIEWS, submissionStatusLabel, canBulkReview, filterSubmissions, filterTakedowns, ownerLabel } = mod;

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

assert(RES_KINDS.every((k) => k.ready), 'all four kinds ready');
assert(kindLabel('mcp') === 'MCP' && kindLabel('secret') === '密钥', 'kind labels');

assert(secretRefsIn('${secret:B} ${secret:A} ${secret:B} ${secret:bad name}').join() === 'A,B', 'refs sorted, deduped, malformed ignored');
assert(secretRefsIn(JSON.stringify(MCP_EXAMPLE)).join() === 'GITHUB_TOKEN', 'example references a secret');

const throws = (fn, re, msg) => {
  try { fn(); } catch (e) { assert(re.test(e.message), msg + ' (got: ' + e.message + ')'); return; }
  assert(false, msg + ' (did not throw)');
};
throws(() => buildPutBody('secret', { value: '' }, false), /密钥值/, 'secret required on create');
assert(buildPutBody('secret', { value: '' }, true).value === '', 'secret may be empty on edit');
assert(buildPutBody('secret', { description: ' d ', value: 'v' }, false).description === 'd', 'description trimmed');
throws(() => buildPutBody('mcp', { content: '{' }, false), /JSON/, 'mcp invalid json');
throws(() => buildPutBody('mcp', { content: '[1]' }, false), /对象/, 'mcp must be object');
const mb = buildPutBody('mcp', { content: JSON.stringify(MCP_EXAMPLE) }, false);
assert(mb.content.transport === 'stdio' && !('value' in mb), 'mcp body carries parsed content only');
throws(() => buildPutBody('prompt', { content: '  ' }, false), /Prompt/, 'prompt required');
assert(buildPutBody('prompt', { content: '你好' }, false).content === '你好', 'prompt body');

assert(/请选择/.test(checkSkillFile(null)), 'skill file required');
assert(/tar\.gz/.test(checkSkillFile({ name: 'skill.zip', size: 10 })), 'zip rejected');
assert(checkSkillFile({ name: 'Code-Review.TGZ', size: 10 }) === '', 'tgz ok, case-insensitive');
assert(checkSkillFile({ name: 'x.tar.gz', size: 10 }) === '', 'tar.gz ok');
assert(/4 MiB/.test(checkSkillFile({ name: 'x.tar.gz', size: MAX_SKILL_UPLOAD + 1 })), 'oversized rejected');
assert(humanSize(512) === '512 B' && humanSize(2048) === '2 KiB' && humanSize(3 << 20) === '3.0 MiB', 'humanSize');

assert(RES_VIEWS.map((v) => v.view).join() === 'live,pending,takedowns', 'three resource views');
assert(submissionStatusLabel('pending') === '待审核' && submissionStatusLabel('approved') === '已批准', 'submission status labels');
assert(canBulkReview({ kind: 'secret', status: 'pending' }), 'secret bulkable');
assert(!canBulkReview({ kind: 'skill', status: 'pending' }), 'skill not bulkable');
assert(!canBulkReview({ kind: 'prompt', status: 'approved' }), 'only pending bulkable');
assert(ownerLabel({ owner_peer: 'alice' }) === 'alice' && ownerLabel({}) === 'operator', 'owner label');
assert(filterSubmissions([{ name: 'MCP', description: 'x', submitted_by: 'alice' }], 'alice').length === 1, 'filter submissions by peer');
assert(filterTakedowns([{ name: 'S', reason: 'old', requested_by: 'bob' }], 'old').length === 1, 'filter takedowns by reason');

console.log('ok resources-state');
