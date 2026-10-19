import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

const load = path => import(`data:text/javascript;base64,${readFileSync(new URL(path, import.meta.url)).toString('base64')}`);
const { H265Player, splitAnnexB, parseHEVCSPS, hevcDecoderConfig } = await load('./web/h265.js');
const { H264Player } = await load('./web/h264.js');
const h264Bytes = new Uint8Array(readFileSync(new URL('../testdata/passthrough.h264', import.meta.url)));
const units = splitAnnexB(new Uint8Array(readFileSync(new URL('../testdata/passthrough.hevc', import.meta.url))));
const sets = [32, 33, 34].map(type => units.find(n => (n[0] >> 1 & 63) === type));
const idr = units.find(n => (n[0] >> 1 & 63) >= 16 && (n[0] >> 1 & 63) <= 21);
const au = (...nals) => new Uint8Array(nals.flatMap(n => [0, 0, 0, 1, ...n]));
const instances = [];
class Decoder {
  static async isConfigSupported(config) { return { supported: config.codec.includes('.2.'), config }; }
  constructor(callbacks) { this.callbacks = callbacks; this.state = 'unconfigured'; this.decodeQueueSize = 0; this.chunks = []; instances.push(this); }
  configure(config) { this.config = config; this.state = 'configured'; }
  decode(chunk) { this.chunks.push(chunk); }
  close() { this.state = 'closed'; }
}
globalThis.VideoDecoder = Decoder;
globalThis.EncodedVideoChunk = class { constructor(config) { Object.assign(this, config); } };

test('H.264 native access units use actual SPS profile and preserve compressed bytes', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const player = new H264Player(() => {}, reason => assert.fail(reason));
  player.setSize(64, 48);
  player.decode(h264Bytes, 1000000);
  assert.equal(player._decoder.config.codec, 'avc1.42c00a');
  assert.deepEqual(player._decoder.chunks[0].data, h264Bytes);
  assert.equal(player._decoder.chunks[0].type, 'key');
  player.reset();
});

test('H.264 backpressure preserves the first output and requests an IDR once', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let requests = 0;
  const player = new H264Player(() => {}, reason => assert.fail(reason), () => requests++);
  player.decode(h264Bytes, 1000000);
  const first = player._decoder;
  first.decodeQueueSize = 32;
  player.decode(au(new Uint8Array([0x41, 1])), 1016667);
  player.decode(h264Bytes, 1033334);
  assert.equal(player._decoder, first);
  assert.equal(first.state, 'configured');
  assert.equal(requests, 1);
  first.callbacks.output({ timestamp: first.chunks[0].timestamp, close() {} });
  first.decodeQueueSize = 0;
  player.decode(h264Bytes, 1050001);
  assert.notEqual(player._decoder, first);
  assert.equal(first.state, 'closed');
  let closed = false;
  first.callbacks.output({ close() { closed = true; } });
  assert.equal(closed, true);
  player.reset();
});

test('H.264 stalled startup asks for a keyframe and rebuilds on that key', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const oldPerformance = globalThis.performance;
  let now = 10;
  globalThis.performance = { now: () => now };
  t.after(() => { globalThis.performance = oldPerformance; });
  let requests = 0;
  const player = new H264Player(() => {}, reason => assert.fail(reason), () => requests++);
  player.decode(h264Bytes, 1000000);
  const first = player._decoder;
  // Continuous input distinguishes a stalled decoder from an idle sender.
  now = 2510; player.decode(au(new Uint8Array([0x41, 1])), 3500000);
  now = 3010;
  t.mock.timers.tick(3000);
  assert.equal(requests, 1);
  player.decode(h264Bytes, 4000000);
  assert.notEqual(player._decoder, first);
  player.reset();
});

test('HEVC SPS determines dimensions, profile and hvcC parameter arrays', () => {
  const info = parseHEVCSPS(sets[1]);
  assert.equal(info.width, 64); assert.equal(info.height, 48);
  assert.equal(info.codec, 'hvc1.1.6.L30.90');
  const config = hevcDecoderConfig(sets);
  assert.equal(config.description[0], 1); assert.equal(config.description[21] & 3, 3);
  assert.equal(config.description[22], 3);
  let offset = 23;
  const view = new DataView(config.description.buffer);
  for (let i = 0; i < 3; i++) {
    assert.equal(config.description[offset++] & 63, 32 + i);
    assert.equal(view.getUint16(offset), 1); offset += 2;
    const length = view.getUint16(offset); offset += 2;
    assert.deepEqual(config.description.slice(offset, offset + length), sets[i]); offset += length;
  }
  assert.equal(offset, config.description.length);
  assert.throws(() => parseHEVCSPS(sets[1].slice(0, 8)));
});

test('HEVC keys configure actual stream and convert NAL lengths without transcoding', () => {
  const player = new H265Player(() => {});
  player.setSize(64, 48);
  player.decode(au(new Uint8Array([2, 1, 9])));
  assert.equal(player._decoder, null);
  player.decode(au(...sets, idr));
  const decoder = player._decoder;
  assert.equal(decoder.config.codec, 'hvc1.1.6.L30.90');
  assert.equal(decoder.chunks[0].type, 'key');
  const chunk = decoder.chunks[0].data;
  const view = new DataView(chunk.buffer);
  let offset = 0;
  for (const nal of [...sets, idr]) {
    const length = view.getUint32(offset); offset += 4;
    assert.deepEqual(chunk.slice(offset, offset + length), nal); offset += length;
  }
  const cra = idr.slice(); cra[0] = 21 << 1;
  player.decode(au(cra));
  assert.equal(player._decoder, decoder); assert.equal(decoder.chunks.at(-1).type, 'key');
  decoder.callbacks.output({ close() {} });
  decoder.decodeQueueSize = 32;
  player.decode(au(new Uint8Array([2, 1, 9])));
  assert.equal(decoder.state, 'configured'); assert.equal(player._waitingKey, true);
  player.decode(au(...sets, idr));
  assert.equal(decoder.state, 'closed'); assert.notEqual(player._decoder, decoder);
  player.setSize(48, 64);
  assert.equal(player._decoder, null);
});

test('HEVC capability probes Main10 and runtime failure renegotiates', async () => {
  assert.equal(await H265Player.supported(), true);
  let failures = 0;
  const player = new H265Player(() => {}, () => failures++);
  player.decode(au(...sets, idr));
  player._decoder.callbacks.error(new Error('Unsupported stream profile'));
  assert.equal(failures, 1); assert.equal(player._decoder, null);
});

test('H264 rejects unsupported capability and reports configure failure', async () => {
  assert.equal(await H264Player.supported(), false);
  let failures = 0;
  const original = Decoder.prototype.configure;
  Decoder.prototype.configure = () => { throw new Error('Unsupported AVC profile'); };
  try {
    const player = new H264Player(() => {}, () => failures++);
    const data = readFileSync(new URL('../testdata/passthrough.h264', import.meta.url));
    player.setSize(64, 48); player.decode(data, 1000000);
    assert.equal(failures, 1); assert.equal(player._decoder, null);
  } finally { Decoder.prototype.configure = original; }
});

test('HEVC stalled output requests a keyframe and recovers without changing codec', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const failures = [];
  let requests = 0;
  const player = new H265Player(() => {}, reason => failures.push(reason), () => requests++);
  player.decode(au(...sets, idr));
  const decoder = player._decoder;
  assert.equal(decoder.chunks.length, 1);
  t.mock.timers.tick(3001);
  assert.deepEqual(failures, []);
  assert.equal(requests, 1); assert.equal(decoder.state, 'configured');
  t.mock.timers.tick(1000);
  assert.equal(requests, 2);
  player.decode(au(...sets, idr));
  assert.notEqual(player._decoder, decoder); assert.equal(decoder.state, 'closed');
  assert.equal(player._waitingKey, false);
  player._decoder.callbacks.output({ close() {} });
  t.mock.timers.tick(5000);
  assert.equal(requests, 2); assert.deepEqual(failures, []);
  player.reset();
});

test('HEVC output cancels fallback and obsolete decoder output is released', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let outputs = 0, failures = 0, released = 0;
  const player = new H265Player(() => outputs++, () => failures++);
  player.decode(au(...sets, idr));
  const decoder = player._decoder;
  decoder.callbacks.output({ close: () => released++ });
  t.mock.timers.tick(3001);
  assert.equal(outputs, 1); assert.equal(failures, 0);
  player.reset();
  decoder.callbacks.output({ close: () => released++ });
  assert.equal(outputs, 1); assert.equal(released, 1);
});

test('HEVC startup under backpressure preserves decoder until its first output', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const player = new H265Player(() => {});
  player.decode(au(...sets, idr));
  const decoder = player._decoder;
  decoder.decodeQueueSize = 32;
  for (let i = 0; i < 30; i++) player.decode(au(new Uint8Array([2, 1, 9])));
  player.decode(au(...sets, idr));
  assert.equal(player._decoder, decoder);
  assert.equal(decoder.state, 'configured'); assert.equal(decoder.chunks.length, 1);
  decoder.callbacks.output({ close() {} });
  player.decode(au(...sets, idr));
  assert.equal(decoder.state, 'closed'); assert.notEqual(player._decoder, decoder);
  player.reset();
});

test('HEVC overflow requests a new IRAP instead of freezing after the first picture', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let outputs = 0, requests = 0;
  const player = new H265Player(() => outputs++, () => assert.fail('must stay in HEVC'), () => requests++);
  player.decode(au(...sets, idr));
  const first = player._decoder;
  first.callbacks.output({ close() {} });
  first.decodeQueueSize = 32;
  for (let i = 0; i < 40; i++) player.decode(au(new Uint8Array([2, 1, 9])));
  assert.equal(requests, 1); assert.equal(first.chunks.length, 1);
  player.decode(au(...sets, idr));
  const recovered = player._decoder;
  recovered.callbacks.output({ close() {} });
  player.decode(au(new Uint8Array([2, 1, 9])));
  recovered.callbacks.output({ close() {} });
  assert.equal(outputs, 3); assert.equal(recovered.chunks.length, 2);
  t.mock.timers.tick(5000);
  assert.equal(requests, 1);
  player.reset();
});

test('HEVC transient queue bursts drain without dropping reference frames', t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let now = 0, requests = 0;
  t.mock.method(performance, 'now', () => now);
  const player = new H265Player(() => {}, () => assert.fail('must stay in HEVC'), () => requests++);
  player.decode(au(...sets, idr));
  const decoder = player._decoder;
  decoder.callbacks.output({ close() {} });
  decoder.decodeQueueSize = 9;
  player.decode(au(new Uint8Array([2, 1, 9])));
  now = 100;
  player.decode(au(new Uint8Array([2, 1, 9])));
  assert.equal(requests, 0);
  assert.equal(decoder.chunks.length, 3);
  decoder.decodeQueueSize = 0;
  now = 200;
  player.decode(au(new Uint8Array([2, 1, 9])));
  decoder.decodeQueueSize = 9;
  now = 300;
  player.decode(au(new Uint8Array([2, 1, 9])));
  now = 549;
  player.decode(au(new Uint8Array([2, 1, 9])));
  assert.equal(requests, 0);
  now = 550;
  player.decode(au(new Uint8Array([2, 1, 9])));
  assert.equal(requests, 1);
  assert.equal(player._waitingKey, true);
  assert.equal(decoder.chunks.length, 6);
  const stats = player.takeDiagnostics();
  assert.equal(stats.received, 7); assert.equal(stats.submitted, 6);
  assert.equal(stats.output, 1); assert.equal(stats.discarded, 1);
  assert.equal(stats.queueMax, 9); assert.equal(stats.pressureRecoveries, 1);
  assert.equal(stats.keyRequests, 1); assert.equal(stats.rebuilds, 1);
  assert.equal(player.takeDiagnostics().received, 0);
  player.reset();
});
