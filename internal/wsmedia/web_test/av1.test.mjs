import test from 'node:test';
import assert from 'node:assert/strict';
import { AV1Player } from '../web/av1.js';

function payload(key, codec = 'av01.0.08M.08') {
  const text = new TextEncoder().encode(key ? codec : '');
  const p = new Uint8Array(2 + text.length + 3);
  p[0] = key ? 1 : 0; p[1] = text.length; p.set(text, 2);
  p.set([0x32, 1, key ? 0x10 : 0x30], 2 + text.length);
  return p;
}

test('AV1 capability probe handles absent, rejected and supported decoders', async () => {
  delete globalThis.VideoDecoder;
  assert.equal(await AV1Player.supported(), false);
  globalThis.VideoDecoder = class { static async isConfigSupported() { return { supported: false }; } };
  assert.equal(await AV1Player.supported(), false);
  VideoDecoder.isConfigSupported = async () => { throw new Error('unsupported'); };
  assert.equal(await AV1Player.supported(), false);
  VideoDecoder.isConfigSupported = async () => ({ supported: true });
  assert.equal(await AV1Player.supported(), true);
});

test('AV1 waits for recovery and resets dependencies after congestion or reconnect', () => {
  const decoders = [];
  globalThis.EncodedVideoChunk = class { constructor(options) { Object.assign(this, options); } };
  globalThis.VideoDecoder = class {
    constructor(callbacks) { this.callbacks = callbacks; this.state = 'unconfigured'; this.decodeQueueSize = 0; this.chunks = []; decoders.push(this); }
    configure(config) { this.config = config; this.state = 'configured'; }
    decode(chunk) { this.chunks.push(chunk); }
    close() { this.state = 'closed'; }
  };
  let fallback = 0;
  const player = new AV1Player(() => {}, () => fallback++);
  player.setSize(1280, 720);
  player.decode(payload(false)); assert.equal(decoders.length, 0);
  player.decode(payload(true)); player.decode(payload(false));
  assert.deepEqual(decoders[0].chunks.map(c => c.type), ['key', 'delta']);
  assert.equal(decoders[0].config.codec, 'av01.0.08M.08');
  assert.deepEqual([...decoders[0].chunks[0].data], [0x32, 1, 0x10]);
  decoders[0].decodeQueueSize = 9;
  player.decode(payload(false)); assert.equal(decoders[0].state, 'closed');
  player.decode(payload(true)); assert.equal(decoders.length, 2);
  player.reset(); player.decode(payload(false)); assert.equal(decoders.length, 2);
  player.decode(payload(true)); decoders[2].callbacks.error(new Error('decode failed'));
  assert.equal(fallback, 1);
  assert.equal(decoders[2].state, 'closed');
});
