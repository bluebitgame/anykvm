import test from 'node:test';
import assert from 'node:assert/strict';
import { bindFullscreen } from '../web/fullscreen.js';
import { Keyboard } from '../web/keyboard.js';

function element() {
  return {
    events: new Map(), attributes: {},
    addEventListener(name, handler) { this.events.set(name, handler); },
    setAttribute(name, value) { this.attributes[name] = value; },
    contains(node) { return this === node; },
  };
}

function setup(webkit = false, rejectExit = false, browserKeyboard = { lock: async () => {}, unlock() {} }) {
  const root = element(), button = element(), notice = element();
  const doc = element();
  doc.documentElement = root;
  const property = webkit ? 'webkitFullscreenElement' : 'fullscreenElement';
  const event = webkit ? 'webkitfullscreenchange' : 'fullscreenchange';
  doc[property] = null;
  doc[webkit ? 'webkitFullscreenEnabled' : 'fullscreenEnabled'] = true;
  root[webkit ? 'webkitRequestFullscreen' : 'requestFullscreen'] = function(options) {
    assert.equal(this, root);
    assert.equal(options.navigationUI, 'hide');
    doc[property] = root;
    doc.events.get(event)();
    return webkit ? undefined : Promise.resolve();
  };
  doc[webkit ? 'webkitExitFullscreen' : 'exitFullscreen'] = function() {
    assert.equal(this, doc);
    if (rejectExit) return Promise.reject(new Error('denied'));
    doc[property] = null;
    doc.events.get(event)();
    return webkit ? undefined : Promise.resolve();
  };
  globalThis.document = doc;
  globalThis.window = element();
  Object.defineProperty(globalThis, 'navigator', { value: { keyboard: browserKeyboard }, configurable: true });
  const changes = [];
  bindFullscreen(button, notice, (...state) => changes.push(state));
  return { root, doc, button, notice, changes, property, event };
}

for (const webkit of [false, true]) {
  test(`${webkit ? 'WebKit' : 'standard'} fullscreen supports entry, button exit and browser exit`, async () => {
    const { doc, root, button, notice, changes, property, event } = setup(webkit);
    assert.equal(button.disabled, false);
    await button.events.get('click')();
    assert.equal(doc[property], root);
    assert.equal(button.attributes['aria-pressed'], 'true');
    assert.equal(button.textContent, '🔳');
    assert.equal(root.attributes['data-fullscreen-mode'], 'toolbar');
    assert.equal(notice.hidden, true);
    await button.events.get('click')();
    assert.equal(doc[property], null);
    assert.equal(button.attributes['aria-pressed'], 'false');
    await button.events.get('click')();
    // Esc/browser UI changes fullscreen independently of the toolbar button.
    doc[property] = null;
    doc.events.get(event)();
    assert.equal(button.textContent, '🔲');
    assert.equal(root.attributes['data-fullscreen-mode'], '');
    assert.deepEqual(changes, [[true, true], [false, false], [true, true], [false, false]]);
  });
}

test('rejected fullscreen requests show a notice and allow retry', async t => {
  t.mock.method(console, 'warn', () => {});
  const { button, notice } = setup(false, true);
  await button.events.get('click')();
  await button.events.get('click')();
  assert.equal(notice.hidden, false);
  assert.match(notice.textContent, /Unable to switch fullscreen/);
  assert.equal(button.disabled, false);
  assert.equal(button.attributes['aria-pressed'], 'true');
});

test('fullscreen locks only Escape and releases the lock on exit', async () => {
  const calls = [];
  const { button } = setup(false, false, {
    async lock(keys) { calls.push(keys); },
    unlock() { calls.push('unlock'); },
  });
  await button.events.get('click')();
  assert.deepEqual(calls, [['Escape']]);
  await button.events.get('click')();
  assert.deepEqual(calls, [['Escape'], 'unlock']);
});

test('missing or denied keyboard lock reports the Escape limitation without breaking fullscreen', async t => {
  t.mock.method(console, 'warn', () => {});
  for (const keyboard of [undefined, { lock: async () => { throw new Error('denied'); }, unlock() {} }]) {
    const { root, doc, button, notice } = setup(false, false, keyboard || {});
    await button.events.get('click')();
    assert.equal(doc.fullscreenElement, root);
    assert.equal(notice.hidden, false);
    assert.match(notice.textContent, /Esc may exit fullscreen/);
    await button.events.get('click')();
    assert.equal(doc.fullscreenElement, null);
    assert.equal(notice.hidden, true);
  }
});

test('Escape down and up are forwarded to the remote device in fullscreen', () => {
  const { root, doc } = setup();
  doc.fullscreenElement = root;
  const canvas = element();
  doc.activeElement = canvas;
  const sent = [];
  const keyboard = new Keyboard(canvas, bytes => sent.push(bytes));
  keyboard.grab();
  const event = { code: 'Escape', preventDefault() {}, stopPropagation() {} };
  window.events.get('keydown')(event);
  window.events.get('keyup')(event);
  assert.deepEqual(sent.map(bytes => ({ down: bytes[1], code: new TextDecoder().decode(bytes.subarray(2)) })),
    [{ down: 1, code: 'Escape' }, { down: 0, code: 'Escape' }]);
});

test('unsupported or policy-disabled fullscreen leaves the button disabled', async () => {
  for (const policyDisabled of [false, true]) {
    const doc = element(), root = element(), button = element();
    doc.documentElement = root;
    if (policyDisabled) {
      root.requestFullscreen = () => assert.fail('must not request fullscreen');
      doc.exitFullscreen = () => {};
      doc.fullscreenEnabled = false;
    }
    globalThis.document = doc;
    globalThis.window = element();
    bindFullscreen(button, element(), () => assert.fail('must not change fullscreen'));
    assert.equal(button.disabled, true);
    await button.events.get('click')();
  }
});

test('Ctrl+Alt+Shift+X toggles locally, ignores repeats and leaves other shortcuts alone', async () => {
  const { root, doc, button } = setup();
  let prevented = 0, stopped = 0;
  const shortcut = {
    code: 'KeyX', ctrlKey: true, altKey: true, shiftKey: true, metaKey: false,
    preventDefault() { prevented++; }, stopImmediatePropagation() { stopped++; },
  };
  const keydown = window.events.get('keydown');
  keydown(shortcut);
  assert.equal(doc.fullscreenElement, root);
  assert.equal(root.attributes['data-fullscreen-mode'], 'video');
  await Promise.resolve();
  keydown({ ...shortcut, repeat: true });
  assert.equal(doc.fullscreenElement, root);
  keydown({ ...shortcut, altKey: false });
  keydown({ ...shortcut, shiftKey: false });
  keydown({ ...shortcut, metaKey: true });
  keydown({ ...shortcut, code: 'Escape' });
  assert.equal(prevented, 2);
  assert.equal(stopped, 2);
  keydown(shortcut);
  assert.equal(doc.fullscreenElement, null);
  assert.equal(root.attributes['data-fullscreen-mode'], '');
  assert.equal(button.attributes['aria-keyshortcuts'], undefined);
});

for (const webkit of [false, true]) {
  test(`${webkit ? 'WebKit' : 'standard'} toolbar fullscreen switches to video fullscreen without exiting`, async () => {
    const { root, doc, button, property, changes } = setup(webkit);
    const shortcut = {
      code: 'KeyX', ctrlKey: true, altKey: true, shiftKey: true, metaKey: false,
      preventDefault() {}, stopImmediatePropagation() {},
    };
    await button.events.get('click')();
    assert.equal(root.attributes['data-fullscreen-mode'], 'toolbar');
    window.events.get('keydown')(shortcut);
    assert.equal(doc[property], root);
    assert.equal(root.attributes['data-fullscreen-mode'], 'video');
    assert.deepEqual(changes, [[true, true], [true, true]]);
    // The button's action stays distinct even when invoked from video mode.
    await button.events.get('click')();
    assert.equal(doc[property], root);
    assert.equal(root.attributes['data-fullscreen-mode'], 'toolbar');
    window.events.get('keydown')(shortcut);
    window.events.get('keydown')(shortcut);
    assert.equal(doc[property], null);
    assert.equal(root.attributes['data-fullscreen-mode'], '');
  });
}
