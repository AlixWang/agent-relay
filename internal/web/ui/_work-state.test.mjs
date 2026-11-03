// Pure state tests for the work queue page.
//
//   node internal/web/ui/_work-state.test.mjs
//
// The page's list rendering depends on the same stage vocabulary the Go
// projector writes. This test pins labels, tones and filter parameters; stage
// ordering is deliberately not redefined here — it comes from internal/stages.
import assert from 'node:assert/strict';
import { stageMeta, workFilterParams, STAGE_META } from './js/work.js';

const expected = [
  'person/approval', 'person/answer', 'person/review', 'person/blocked', 'person/failed', 'person/offline',
  'working', 'idle', 'queued', 'done', 'cancelled',
];
for (const stage of expected) {
  assert.ok(STAGE_META[stage], `missing stage ${stage}`);
  const m = stageMeta(stage);
  assert.equal(typeof m.label, 'string');
  assert.ok(m.label.length > 0, `empty label for ${stage}`);
  assert.ok(['warn', 'bad', 'brand', 'mono', 'ok'].includes(m.tone), `bad tone for ${stage}: ${m.tone}`);
}

assert.equal(stageMeta('unknown').label, 'unknown');
assert.equal(stageMeta('').label, '未知');

assert.equal(workFilterParams('attention', '').get('stage'), 'attention');
assert.equal(workFilterParams('working', '').get('stage'), 'working');
assert.equal(workFilterParams('done', '').get('state'), 'done');
assert.equal(workFilterParams('all', '').get('state'), null);
assert.equal(workFilterParams('attention', 'muse').get('q'), 'muse');

console.log('work state: all checks passed');
