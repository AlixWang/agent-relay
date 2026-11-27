// Behavioural test for the shared input/confirm dialogs.
//   node internal/web/ui/_prompt-input.test.mjs
//
// Imports the real utils module with a stubbed document/window, so a syntax
// or import error also fails the lane. Covers the returnValue reset: a
// <dialog> keeps returnValue across opens, so an Esc close after a previous
// OK must still resolve as a cancel, never as a save.

import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

const assert = (cond, msg) => {
  if (!cond) {
    console.error('FAIL: ' + msg);
    process.exit(1);
  }
};

function mockDialog() {
  const listeners = new Map();
  const dlg = {
    returnValue: '',
    open: false,
    shown: 0,
    closed: 0,
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(fn);
    },
    removeEventListener(type, fn) {
      listeners.get(type)?.delete(fn);
    },
    listenerCount(type) { return listeners.get(type)?.size || 0; },
    showModal() {
      if (this.open) throw new Error('InvalidStateError: already open');
      this.open = true;
      this.shown++;
    },
    close() {
      this.open = false;
      this.closed++;
      [...(listeners.get('close') || [])].forEach((fn) => fn());
    },
    querySelectorAll() { return []; },
  };
  return dlg;
}

function mockInput() {
  return {
    value: '',
    placeholder: '',
    attrs: {},
    focused: false,
    selected: false,
    focus() { this.focused = true; },
    select() { this.selected = true; },
    setAttribute(k, v) { this.attrs[k] = v; },
  };
}

// Per-test element map for document.getElementById.
let elements = {};
let promptCalls = [];
globalThis.document = { getElementById: (id) => elements[id] ?? null };
globalThis.window = {
  prompt: (...args) => {
    promptCalls.push(args);
    return 'fallback-value';
  },
};

const mod = await import(pathToFileURL(path.join(here, 'js', 'utils.js')).href);
const { promptInput, confirmAction } = mod;

function setupPromptDialog() {
  const dlg = mockDialog();
  const input = mockInput();
  elements = {
    promptDlg: dlg,
    prTitle: { textContent: '' },
    prHint: { textContent: '' },
    prInput: input,
    prOk: { textContent: '' },
  };
  return { dlg, input };
}

// 1. OK resolves the input value.
{
  const { dlg, input } = setupPromptDialog();
  const p = promptInput({ title: 'T', value: 'a,b' });
  assert(dlg.open, 'dialog opens on promptInput');
  assert(input.focused && input.selected, 'input focused and selected');
  input.value = 'a,b,c';
  dlg.returnValue = 'ok';
  dlg.close();
  assert((await p) === 'a,b,c', 'OK resolves input value');
  assert(dlg.listenerCount('close') === 0, 'close listener removed after OK');
}

// 2. Cancel resolves null.
{
  const { dlg } = setupPromptDialog();
  const p = promptInput({ title: 'T', value: 'x' });
  dlg.returnValue = 'cancel';
  dlg.close();
  assert((await p) === null, 'Cancel resolves null');
}

// 3. P0 regression: OK once, then Esc (close without touching returnValue)
// must resolve null, not the previous OK.
{
  const { dlg, input } = setupPromptDialog();
  const first = promptInput({ title: 'T', value: 'scope-a' });
  dlg.returnValue = 'ok';
  dlg.close();
  assert((await first) === 'scope-a', 'first OK resolves value');
  const second = promptInput({ title: 'T', value: 'scope-b' });
  assert(dlg.returnValue === '', 'returnValue reset before second open');
  input.value = 'scope-b';
  dlg.close(); // Esc / backdrop: returnValue untouched by the close itself
  assert((await second) === null, 'Esc after a previous OK resolves null');
}

// 4. Already-open dialog resolves null without re-opening.
{
  const { dlg } = setupPromptDialog();
  dlg.open = true;
  const shownBefore = dlg.shown;
  assert((await promptInput({ title: 'T' })) === null, 'open dialog resolves null');
  assert(dlg.shown === shownBefore, 'showModal not called on an open dialog');
}

// 5. Missing dialog falls back to window.prompt with title + hint.
{
  elements = {};
  promptCalls = [];
  assert((await promptInput({ title: 'T', hint: 'H', value: 'v' })) === 'fallback-value', 'fallback returns window.prompt value');
  assert(promptCalls.length === 1 && promptCalls[0][0] === 'T\nH' && promptCalls[0][1] === 'v', 'fallback prompt gets title, hint and value');
}

// 6. confirmAction has the same reset: OK once, then Esc must be false.
{
  const dlg = mockDialog();
  elements = {
    confirmDlg: dlg,
    cfTitle: { textContent: '' },
    cfBody: { textContent: '' },
    cfOk: { textContent: '', className: '' },
    cfIc: { setAttribute() {}, querySelector: () => ({ setAttribute() {} }) },
  };
  const first = confirmAction({ title: 'Sure?' });
  dlg.returnValue = 'ok';
  dlg.close();
  assert((await first) === true, 'confirm OK resolves true');
  const second = confirmAction({ title: 'Sure?' });
  assert(dlg.returnValue === '', 'confirm returnValue reset before second open');
  dlg.close();
  assert((await second) === false, 'confirm Esc after a previous OK resolves false');
}

console.log('prompt input dialog tests passed');
