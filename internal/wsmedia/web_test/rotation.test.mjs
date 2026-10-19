import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fitRotatedFrame, inverseRotation } from '../web/rotation.js';
import { Pointer } from '../web/pointer.js';

test('rotated phone corners map back to the original touchscreen', () => {
  const corners = [[0, 0], [1, 0], [0, 1], [1, 1]];
  const expected = {
    0: [[0, 0], [1, 0], [0, 1], [1, 1]],
    90: [[0, 1], [0, 0], [1, 1], [1, 0]],
    180: [[1, 1], [0, 1], [1, 0], [0, 0]],
    270: [[1, 0], [1, 1], [0, 0], [0, 1]],
  };
  for (const rotation of [0, 90, 180, 270]) {
    corners.forEach(([x, y], i) => assert.deepEqual(inverseRotation(x, y, rotation), { x: expected[rotation][i][0], y: expected[rotation][i][1] }));
    assert.deepEqual(inverseRotation(0.5, 0.5, rotation), { x: 0.5, y: 0.5 });
  }
});

test('landscape layout fits both browser edges without changing encoded size', () => {
  for (const rotation of [0, 90, 180, 270]) {
    const fit = fitRotatedFrame(1328, 2896, rotation, 1280, 680);
    assert.ok(fit.displayWidth <= 1280 && fit.displayHeight <= 680);
    assert.ok(Math.abs(fit.width / fit.height - 1328 / 2896) < 1e-6);
    assert.ok(Math.abs(Math.max(fit.displayWidth / 1280, fit.displayHeight / 680) - 1) < 1e-6);
  }
});

function surface(width = 100, height = 100) {
  return {
    width, height, style: {}, events: new Map(), setAttribute() {},
    addEventListener(name, callback) { this.events.set(name, callback); },
    removeEventListener() {},
    getBoundingClientRect() { return { left: 0, top: 0, width: this.width, height: this.height }; },
    getContext() { return { drawImage() {} }; },
  };
}

test('rotated mouse and touch send original framebuffer coordinates', () => {
  globalThis.window = surface();
  const canvas = surface(128, 64);
  const sent = [];
  new Pointer(canvas, bytes => sent.push(bytes), () => ({ w: 64, h: 128, rotation: 90 }));
  const point = () => ({ x: sent.at(-1)[2] * 256 + sent.at(-1)[3], y: sent.at(-1)[4] * 256 + sent.at(-1)[5] });
  canvas.events.get('mousedown')({ buttons: 1, clientX: 0, clientY: 0, preventDefault() {} });
  assert.deepEqual(point(), { x: 0, y: 127 });
  canvas.events.get('touchmove')({ touches: [{ clientX: 128, clientY: 64 }], preventDefault() {} });
  assert.deepEqual(point(), { x: 63, y: 0 });
  canvas.events.get('touchend')({ preventDefault() {} });
  assert.equal(sent.at(-1)[1], 0);
});

test('orientation-only metadata preserves the active HEVC decoder and releases touches', async t => {
  t.mock.timers.enable({ apis: ['setTimeout', 'setInterval'] });
  const canvas = surface(64, 48), wrap = surface(800, 600);
  globalThis.window = surface();
  window.isSecureContext = true;
  globalThis.document = { documentElement: surface(), getElementById: id => id === 'wrap' ? wrap : canvas, addEventListener() {} };
  globalThis.location = { pathname: '/target/test', search: '?codec=hevc', protocol: 'http:', host: '127.0.0.1' };
  const decoders = [];
  globalThis.VideoDecoder = class {
    static async isConfigSupported() { return { supported: true }; }
    constructor(callbacks) { this.callbacks = callbacks; this.decodeQueueSize = 0; this.state = 'unconfigured'; decoders.push(this); }
    configure() { this.state = 'configured'; }
    decode() {}
    close() { this.state = 'closed'; }
  };
  globalThis.EncodedVideoChunk = class { constructor(config) { Object.assign(this, config); } };
  let socket;
  globalThis.WebSocket = class {
    constructor() { socket = this; this.readyState = 1; this.sent = []; }
    send(bytes) { this.sent.push(bytes); }
  };
  await import('../web/app.js');
  const metadata = rotation => socket.onmessage({ data: new Uint8Array([0, ...new TextEncoder().encode(JSON.stringify({ w: 64, h: 48, rotation }))]).buffer });
  metadata(0);
  const bytes = readFileSync(new URL('../../testdata/passthrough.hevc', import.meta.url));
  socket.onmessage({ data: new Uint8Array([0x11, ...bytes]).buffer });
  assert.equal(decoders.length, 1);
  for (const rotation of [90, 180, 270, 0]) {
    canvas.events.get('mousedown')({ buttons: 1, clientX: 20, clientY: 20, preventDefault() {} });
    metadata(rotation);
    assert.equal(socket.sent.at(-1)[1], 0);
    assert.equal(canvas.style.transform, `rotate(${rotation}deg)`);
    assert.equal(canvas.width, 64); assert.equal(canvas.height, 48);
    assert.equal(decoders.length, 1); assert.equal(decoders[0].state, 'configured');
  }
  socket.onclose();
});
