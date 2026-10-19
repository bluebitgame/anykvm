import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { H265Player } from '../web/h265.js';
import { Pointer } from '../web/pointer.js';

function surface() {
  const events = new Map();
  return {
    events,
    width: 100, height: 100,
    style: {},
    setAttribute() {},
    addEventListener(name, handler) { events.set(name, handler); },
    removeEventListener() {},
    getBoundingClientRect() { return { left: 0, top: 0, width: 100, height: 100 }; },
    getContext() { return {}; },
  };
}

test('HEVC capability reflects codec support, including probe failure', async () => {
  delete globalThis.VideoDecoder;
  assert.equal(await H265Player.supported(), false);
  globalThis.VideoDecoder = class {
    static async isConfigSupported() { return { supported: false }; }
  };
  assert.equal(await H265Player.supported(), false);
  globalThis.VideoDecoder.isConfigSupported = async () => { throw new Error('unsupported'); };
  assert.equal(await H265Player.supported(), false);
  globalThis.VideoDecoder.isConfigSupported = async () => ({ supported: true });
  assert.equal(await H265Player.supported(), true);
});

test('mouse release outside the canvas and focus loss clear held buttons', () => {
  const canvas = surface();
  globalThis.window = surface();
  const sent = [];
  new Pointer(canvas, bytes => sent.push(bytes), () => ({ w: 100, h: 100 }));
  const event = buttons => ({ buttons, clientX: 200, clientY: 50, preventDefault() {} });
  canvas.events.get('mousedown')(event(2));
  assert.equal(sent.at(-1)[1], 4); // browser right button -> RFB bit 2
  window.events.get('mouseup')(event(0));
  assert.equal(sent.at(-1)[1], 0);
  canvas.events.get('mousedown')(event(1));
  window.events.get('blur')();
  assert.equal(sent.at(-1)[1], 0);
});

test('raw absolute mouse motion forwards without a rendering tick and coalesces bursts without duplicate mousemove', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let now = 0;
  t.mock.method(performance, 'now', () => now);
  globalThis.window = surface();
  window.isSecureContext = true;
  window.onpointerrawupdate = null;
  const canvas = surface(), sent = [];
  new Pointer(canvas, bytes => sent.push(bytes), () => ({ w: 100, h: 100 }));
  const event = (type, x, pointerType = 'mouse', buttons = 0) => ({
    type, clientX: x, clientY: 50, pointerType, buttons, preventDefault() {},
  });
  for (let x = 0; x < 100; x++) {
    canvas.events.get('pointerrawupdate')(event('pointerrawupdate', x));
  }
  assert.equal(sent.length, 1); // First sample is immediate; never wait for rAF.
  now = 4;
  t.mock.timers.tick(4);
  assert.equal(sent.length, 2); // Only the latest of the remaining 99 positions.
  canvas.events.get('mousemove')(event('mousemove', 99));
  canvas.events.get('pointerrawupdate')(event('pointerrawupdate', 70, 'touch'));
  canvas.events.get('pointerrawupdate')(event('pointerrawupdate', 70, 'pen'));
  assert.equal(sent.length, 2);
  const last = new DataView(sent.at(-1).buffer);
  assert.equal(last.getUint16(2), 98);
  canvas.events.get('mousedown')(event('mousedown', 99, 'mouse', 1));
  assert.equal(sent.at(-1)[1], 1);
  window.events.get('mouseup')(event('mouseup', 99));
  assert.equal(sent.at(-1)[1], 0);
});

test('raw-capable browsers retain relative mouse deltas and the nonsecure fallback', () => {
  globalThis.window = surface();
  window.isSecureContext = true;
  window.onpointerrawupdate = null;
  globalThis.document = { pointerLockElement: null, addEventListener() {} };
  const canvas = surface(), sent = [];
  const pointer = new Pointer(canvas, bytes => sent.push(bytes), () => ({ w: 100, h: 100 }));
  document.pointerLockElement = canvas;
  pointer.setMode('relative');
  const event = type => ({ type, pointerType: 'mouse', buttons: 0, clientX: 50, clientY: 50, movementX: 3, movementY: -2 });
  canvas.events.get('pointerrawupdate')(event('pointerrawupdate'));
  assert.equal(sent.length, 0);
  canvas.events.get('mousemove')(event('mousemove'));
  assert.equal(sent.length, 1);
  assert.equal(sent[0][0], 0x87);
  assert.equal(new DataView(sent[0].buffer).getInt16(2), 3);
  assert.equal(new DataView(sent[0].buffer).getInt16(4), -2);
  document.pointerLockElement = null;
  window.isSecureContext = false;
  const fallback = surface();
  new Pointer(fallback, bytes => sent.push(bytes), () => ({ w: 100, h: 100 }));
  assert.equal(fallback.events.has('pointerrawupdate'), false);
  fallback.events.get('mousemove')(event('mousemove'));
  assert.equal(sent.length, 2);
});

test('forced H.264 suppresses HEVC even when both decoders are available', async () => {
  const canvas = surface();
  const contexts = [];
  canvas.getContext = (kind, options) => { contexts.push({ kind, options }); return {}; };
  globalThis.window = surface();
  window.VideoDecoder = globalThis.VideoDecoder;
  const audioButton = surface();
  globalThis.document = { documentElement: surface(), getElementById: id => id === 'audioBtn' ? audioButton : canvas, addEventListener() {} };
  globalThis.location = { pathname: '/target/test', search: '?codec=h264', protocol: 'https:', host: 'localhost' };
  const intervals = globalThis.setInterval;
  globalThis.setInterval = () => 0;
  let socket;
  globalThis.WebSocket = class {
    constructor() { socket = this; this.readyState = 1; }
    send(bytes) { this.sent = bytes; }
  };
  try {
    await import('../web/app.js');
    assert.deepEqual(contexts, [{ kind: '2d', options: { alpha: false, desynchronized: false } }]);
    socket.onopen();
    const caps = JSON.parse(new TextDecoder().decode(socket.sent.subarray(1)));
    assert.deepEqual(caps, { h264: true, hevc: false, av1: false, audioCodecs: [], audioFormats: [], rawPointer: false });
    // This decoder stub has no configure(): a runtime failure must keep the
    // explicit native H.264 request instead of renegotiating JPEG/HEVC.
    const hello = socket.sent;
    const bytes = readFileSync(new URL('../../testdata/passthrough.h264', import.meta.url));
    const message = new Uint8Array(9 + bytes.length);
    message[0] = 0x10;
    new DataView(message.buffer).setBigUint64(1, BigInt(Date.now()) * 1000n, true);
    message.set(bytes, 9);
    socket.onmessage({ data: message.buffer });
    assert.equal(socket.sent, hello);
    assert.equal(audioButton.textContent, '🔈');
    assert.equal(audioButton.disabled, true);
    window.AudioContext = class {
      constructor() { this.state = 'suspended'; }
      addEventListener() {}
      async resume() { this.state = 'running'; }
      async suspend() { this.state = 'suspended'; }
    };
    const metadata = audioAvailable => ({ data: new Uint8Array([0, ...new TextEncoder().encode(JSON.stringify({ w: 100, h: 100, audioAvailable }))]).buffer });
    socket.onmessage(metadata(true));
    assert.equal(audioButton.textContent, '🔊'); assert.equal(audioButton.disabled, false);
    await audioButton.onclick(); assert.equal(audioButton.textContent, '🔇');
    await audioButton.onclick(); assert.equal(audioButton.textContent, '🔊');
    socket.onmessage(metadata(false));
    assert.equal(audioButton.textContent, '🔈'); assert.equal(audioButton.disabled, true);
    await audioButton.onclick(); assert.equal(audioButton.textContent, '🔈');
  } finally {
    globalThis.setInterval = intervals;
  }
});

test('keyboard releases held keys when focus moves within the page', async () => {
  const { Keyboard } = await import('../web/keyboard.js');
  const target = surface();
  target.contains = node => node === target;
  globalThis.window = surface();
  globalThis.document = { activeElement: target };
  const sent = [];
  const keyboard = new Keyboard(target, bytes => sent.push(bytes));
  keyboard.grab();
  const event = { code: 'KeyA', preventDefault() {}, stopPropagation() {} };
  window.events.get('keydown')(event);
  assert.equal(sent.at(-1)[1], 1);
  target.events.get('focusout')({ relatedTarget: {} });
  assert.equal(sent.at(-1)[1], 0);
  assert.equal(sent.length, 2);
  document.activeElement = {};
  window.events.get('keyup')(event);
  assert.equal(sent.length, 2);
  keyboard.release();
});

test('keyboard keyup releases a tracked key even without focusout delivery', async () => {
  const { Keyboard } = await import('../web/keyboard.js');
  const target = surface();
  target.contains = node => node === target;
  globalThis.window = surface();
  globalThis.document = { activeElement: target };
  const sent = [];
  const keyboard = new Keyboard(target, bytes => sent.push(bytes));
  keyboard.grab();
  const event = { code: 'ShiftLeft', preventDefault() {}, stopPropagation() {} };
  window.events.get('keydown')(event);
  document.activeElement = {};
  window.events.get('keyup')(event);
  assert.deepEqual(sent.map(bytes => bytes[1]), [1, 0]);
  keyboard.release();
});

test('relative mouse sends signed deltas without absolute motion or release jumps', () => {
  const canvas = surface();
  globalThis.window = surface();
  globalThis.document = surface();
  const sent = [];
  const pointer = new Pointer(canvas, bytes => sent.push(bytes), () => ({ w: 200, h: 100 }));
  pointer.setMode('relative');
  assert.equal(sent.length, 0);
  const event = (type, x, y, buttons = 0) => ({ type, clientX: x, clientY: y, buttons, preventDefault() {} });
  const delta = bytes => [bytes[0], bytes[1], new DataView(bytes.buffer).getInt16(2), new DataView(bytes.buffer).getInt16(4)];
  canvas.events.get('mousemove')(event('mousemove', 50, 50));
  canvas.events.get('mousemove')(event('mousemove', 47, 54));
  assert.deepEqual(delta(sent.at(-1)), [0x87, 0, -6, 4]);
  canvas.events.get('mousedown')(event('mousedown', 47, 54, 1));
  window.events.get('mouseup')(event('mouseup', 400, 400));
  assert.deepEqual(delta(sent.at(-1)), [0x87, 0, 0, 0]);
  canvas.events.get('mousemove')(event('mousemove', 47, 54));
  assert.deepEqual(delta(sent.at(-1)), [0x87, 0, 0, 0]);
  canvas.events.get('wheel')({ ...event('wheel', 400, 400), deltaY: -1 });
  assert.deepEqual(sent.slice(-2).map(delta), [[0x87, 8, 0, 0], [0x87, 0, 0, 0]]);
  document.pointerLockElement = canvas;
  document.events.get('pointerlockchange')();
  canvas.events.get('mousemove')({ ...event('mousemove', 0, 0, 1), movementX: -2, movementY: 3 });
  assert.deepEqual(delta(sent.at(-1)), [0x87, 1, -4, 3]);
  document.pointerLockElement = null;
  document.events.get('pointerlockchange')();
  assert.deepEqual(delta(sent.at(-1)), [0x87, 0, 0, 0]);
});

test('relative touch preserves fractional movement and inverse display rotation', () => {
  const canvas = surface();
  globalThis.window = surface(); globalThis.document = surface();
  const sent = [];
  const pointer = new Pointer(canvas, bytes => sent.push(bytes), () => ({ w: 100, h: 100, rotation: 90 }));
  pointer.setMode('relative');
  const touch = x => ({ touches: [{ clientX: x, clientY: 20 }], preventDefault() {} });
  canvas.events.get('touchstart')(touch(10));
  for (const x of [10.25, 10.5, 10.75, 11]) canvas.events.get('touchmove')(touch(x));
  const deltas = sent.map(bytes => [new DataView(bytes.buffer).getInt16(2), new DataView(bytes.buffer).getInt16(4)]);
  assert.equal(deltas.reduce((total, d) => total + d[0], 0), 0);
  assert.equal(deltas.reduce((total, d) => total + d[1], 0), -1);
  canvas.events.get('touchend')({ preventDefault() {} });
  assert.equal(sent.at(-1)[1], 0);
});
