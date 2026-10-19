import test from 'node:test';
import assert from 'node:assert/strict';
import { supportedAudioCodecs, supportedAudioFormats, parseEncodedAudio, CompressedAudioPlayer } from '../web/audio.js';

function packet(extra = {}) {
  const config = { codec: 'mp4a.40.39', config: '+OZQAA==', sampleRate: 48000, channels: 2, timestamp: 10000, frames: 480, ...extra };
  const meta = new TextEncoder().encode(JSON.stringify(config));
  const bytes = new Uint8Array(2 + meta.length + 3);
  new DataView(bytes.buffer).setUint16(0, meta.length); bytes.set(meta, 2); bytes.set([1, 2, 3], 2 + meta.length); return bytes;
}
test('AAC support is checked independently for LC and ELD', async () => {
  delete globalThis.AudioDecoder; assert.deepEqual(await supportedAudioCodecs(), []);
  globalThis.AudioDecoder = class { static async isConfigSupported(c) { return { supported: c.codec === 'mp4a.40.2' }; } };
  assert.deepEqual(await supportedAudioCodecs(), ['mp4a.40.2']);
});
test('AAC-ELD probe distinguishes AirPlay 44.1kHz from CoreDevice 48kHz', async () => {
  globalThis.AudioDecoder = class { static async isConfigSupported(c) { return { supported: c.codec === 'mp4a.40.39' && c.sampleRate === 44100 }; } };
  assert.deepEqual(await supportedAudioFormats(), [{ codec: 'mp4a.40.39', sampleRate: 44100, channels: 2, config: '+OhQAA==', frames: 480 }]);
});
test('compressed audio retains per-packet volume and resets decoder on FLUSH', () => {
  const callbacks = [], gains = [];
  let closed = 0;
  globalThis.AudioDecoder = class {
    constructor(cb) { callbacks.push(cb); this.state = 'configured'; this.decodeQueueSize = 0; }
    configure() {} decode() {} close() { this.state = 'closed'; closed++; }
  };
  globalThis.EncodedAudioChunk = class { constructor(c) { Object.assign(this, c); } };
  const context = { state: 'running', createBuffer: () => ({ getChannelData: () => new Float32Array(480) }) };
  const player = new CompressedAudioPlayer(() => context, (_, gain) => gains.push(gain), reason => assert.fail(reason));
  player.push(packet({ gain: 0, epoch: 1 }));
  player.push(packet({ gain: 0.5, epoch: 1, timestamp: 20000 }));
  const data = timestamp => ({ timestamp, numberOfChannels: 2, numberOfFrames: 480, sampleRate: 48000, copyTo() {}, close() {} });
  callbacks[0].output(data(10000)); callbacks[0].output(data(20000));
  assert.deepEqual(gains, [0, 0.5]);
  player.push(packet({ epoch: 2 })); assert.equal(closed, 1);
  callbacks[0].output(data(10000)); assert.deepEqual(gains, [0, 0.5]);
  player.reset(); assert.equal(closed, 2);
});
test('encoded audio preserves decoder description and raw AU, rejects truncated messages', () => {
  const a = parseEncodedAudio(packet());
  assert.deepEqual([...a.description], [0xf8, 0xe6, 0x50, 0]); assert.deepEqual([...a.data], [1, 2, 3]); assert.equal(a.timestamp, 10000);
  assert.equal(parseEncodedAudio(new Uint8Array([0, 100, 1])), null);
});
test('muted audio skips decoding, decoded data closes, decoder errors request fallback', () => {
  let running = false, played = 0, closed = 0, errors = 0;
  const context = { state: 'running', createBuffer: () => ({ getChannelData: () => new Float32Array(480) }) };
  let callbacks;
  globalThis.AudioDecoder = class {
    constructor(cb) { callbacks = cb; this.state = 'unconfigured'; this.decodeQueueSize = 0; }
    configure(c) { this.state = 'configured'; assert.equal(c.codec, 'mp4a.40.39'); }
    decode(chunk) { assert.equal(chunk.timestamp, 10000); callbacks.output({ numberOfChannels: 2, numberOfFrames: 480, sampleRate: 48000, copyTo() {}, close() { closed++; } }); }
    close() { this.state = 'closed'; }
  };
  globalThis.EncodedAudioChunk = class { constructor(c) { Object.assign(this, c); } };
  const player = new CompressedAudioPlayer(() => running ? context : null, () => played++, () => errors++);
  player.push(packet()); assert.equal(player.decoder, null);
  running = true; player.push(packet()); assert.equal(played, 1); assert.equal(closed, 1);
  callbacks.error(new Error('unsupported')); assert.equal(player.decoder, null); assert.equal(errors, 1);
  player.reset(); assert.equal(errors, 1);
});
