import test from 'node:test';
import assert from 'node:assert/strict';
import { H264Player } from '../web/h264.js';

test('pending output diagnostics track pictures after decodeQueueSize reaches zero', () => {
  const oldPerformance = globalThis.performance;
  let now = 10, decoder;
  globalThis.performance = { now: () => now };
  globalThis.EncodedVideoChunk = class { constructor(options) { Object.assign(this, options); } };
  globalThis.VideoDecoder = class {
    constructor(callbacks) { decoder = this; this.callbacks = callbacks; this.state = 'unconfigured'; this.decodeQueueSize = 0; this.chunks = []; }
    configure() { this.state = 'configured'; }
    decode(chunk) { this.chunks.push(chunk); }
    close() { this.state = 'closed'; }
  };
  const presented = [];
  const player = new H264Player((frame, pts) => presented.push(pts), () => assert.fail('unexpected fallback'));
  const key = new Uint8Array([0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0, 0, 0, 1, 0x65, 0x80]);
  const delta = new Uint8Array([0, 0, 0, 1, 0x41, 0x80]);
  try {
    for (let i = 0; i < 6; i++) { now += 16; player.decode(i === 0 ? key : delta, 1000000 + i * 16667); }
    const stats = player.takeDiagnostics();
    assert.equal(stats.queue, 0);
    assert.equal(stats.pendingOutput, 6);
    assert.equal(stats.pendingMax, 6);
    assert.equal(stats.submitted, 6);
    assert.equal(stats.discarded, 0);
    for (let i = 0; i < 6; i++) {
      decoder.callbacks.output({ timestamp: decoder.chunks[i].timestamp });
      assert.equal(player.takeDiagnostics().pendingOutput, 5 - i);
    }
    assert.deepEqual(presented, decoder.chunks.map(chunk => chunk.timestamp));
    player.decode(delta, 1100002);
    player.reset();
    assert.equal(player.takeDiagnostics().pendingOutput, 0);
  } finally {
    player.reset(); globalThis.performance = oldPerformance;
    delete globalThis.VideoDecoder; delete globalThis.EncodedVideoChunk;
  }
});

test('source PTS reaches WebCodecs and presentation without corrupting decode diagnostics', () => {
  const oldPerformance = globalThis.performance;
  let now = 10;
  globalThis.performance = { now: () => now };
  let decoder;
  globalThis.EncodedVideoChunk = class { constructor(options) { Object.assign(this, options); } };
  globalThis.VideoDecoder = class {
    constructor(callbacks) { decoder = this; this.callbacks = callbacks; this.state = 'unconfigured'; this.decodeQueueSize = 0; this.chunks = []; }
    configure() { this.state = 'configured'; }
    decode(chunk) { this.chunks.push(chunk); }
    close() { this.state = 'closed'; }
  };
  const presented = [];
  const player = new H264Player((frame, pts) => presented.push(pts), () => assert.fail('unexpected failure'));
  const source = 1791655600123456;
  const key = new Uint8Array([0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0, 0, 0, 1, 0x65, 0x80]);
  try {
    player.decode(key, source);
    assert.equal(decoder.chunks[0].timestamp, source);
    now = 17; decoder.callbacks.output({ timestamp: decoder.chunks[0].timestamp });
    now = 20; player.decode(new Uint8Array([0, 0, 0, 1, 0x41, 0x80]), source + 16667);
    assert.equal(decoder.chunks[1].timestamp, source + 16667);
    now = 29; decoder.callbacks.output({ timestamp: decoder.chunks[1].timestamp });
    assert.deepEqual(presented, [source, source + 16667]);
    const stats = player.takeDiagnostics();
    assert.equal(stats.decodeAvgMs, 8); assert.equal(stats.decodeMaxMs, 9);
    assert.equal(player._submissions.size, 0);
    now = 31; player.decode(new Uint8Array([0, 0, 0, 1, 0x41, 0x80]), source + 33334);
    now = 39; decoder.callbacks.output({ timestamp: source + 33335 });
    assert.equal(presented.at(-1), source + 33335, 'valid output PTS survives a metadata lookup miss');
    assert.equal(player.takeDiagnostics().unmatchedOutput, 1);
    player.reset(); player.decode(key, 1000000);
    assert.equal(decoder.chunks[0].timestamp, 1000000);
  } finally {
    player.reset(); globalThis.performance = oldPerformance;
    delete globalThis.VideoDecoder; delete globalThis.EncodedVideoChunk;
  }
});

test('H.264 measures decode delay in the browser clock domain without changing recovery', () => {
  const oldPerformance = globalThis.performance;
  let now = 10;
  globalThis.performance = { now: () => now };
  const decoders = [], output = [];
  globalThis.EncodedVideoChunk = class { constructor(options) { Object.assign(this, options); } };
  globalThis.VideoDecoder = class {
    constructor(callbacks) { this.callbacks = callbacks; this.state = 'unconfigured'; this.decodeQueueSize = 0; this.chunks = []; decoders.push(this); }
    configure(config) { this.config = config; this.state = 'configured'; }
    decode(chunk) { this.chunks.push(chunk); }
    close() { this.state = 'closed'; }
  };
  let requests = 0;
  const player = new H264Player(frame => output.push(frame), () => assert.fail('unexpected decoder fallback'), () => requests++);
  const key = new Uint8Array([0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0, 0, 0, 1, 0x65, 0x80]);
  const delta = new Uint8Array([0, 0, 0, 1, 0x41, 0x80]);
  try {
    player.setSize(608, 1080);
    player.decode(delta, 10000);
    assert.equal(requests, 1);
    player.decode(key, 10000);
    assert.equal(decoders[0].config.optimizeForLatency, true);
    now = 15;
    decoders[0].callbacks.output({ timestamp: decoders[0].chunks[0].timestamp });
    now = 40;
    player.decode(delta, 40000);
    now = 47;
    decoders[0].callbacks.output({ timestamp: decoders[0].chunks[1].timestamp });
    assert.deepEqual(decoders[0].chunks.map(chunk => chunk.timestamp), [10000, 40000]);
    assert.equal(output.length, 2);
    assert.deepEqual(player.takeDiagnostics(), { received: 3, submitted: 2, output: 2, discarded: 1, keyRequests: 1, queueMax: 0, pendingMax: 1, pendingOutput: 0, unmatchedOutput: 0, decodeMaxMs: 7, queue: 0, waitingKey: false, decodeAvgMs: 6, inputGapMaxMs: 0, outputGapMaxMs: 32 });
    assert.equal(player.takeDiagnostics().output, 0);
    player.reset();
    player.decode(delta, 47000);
    assert.equal(decoders.length, 1);
    assert.equal(requests, 2);
  } finally {
    player.reset();
    globalThis.performance = oldPerformance;
    delete globalThis.VideoDecoder;
    delete globalThis.EncodedVideoChunk;
  }
});

test('H.264 resumes deltas after a quiet screen and only recovers sustained decoder stalls', () => {
  const oldPerformance = globalThis.performance;
  let now = 10;
  globalThis.performance = { now: () => now };
  const decoders = [];
  globalThis.EncodedVideoChunk = class { constructor(options) { Object.assign(this, options); } };
  globalThis.VideoDecoder = class {
    constructor(callbacks) { this.callbacks = callbacks; this.state = 'unconfigured'; this.decodeQueueSize = 0; this.chunks = []; decoders.push(this); }
    configure() { this.state = 'configured'; }
    decode(chunk) { this.chunks.push(chunk); }
    close() { this.state = 'closed'; }
  };
  let requests = 0;
  const player = new H264Player(() => {}, () => assert.fail('unexpected fallback'), () => requests++);
  const key = new Uint8Array([0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0, 0, 0, 1, 0x65, 0x80]);
  const delta = new Uint8Array([0, 0, 0, 1, 0x41, 0x80]);
  try {
    player.decode(key, 10000);
    const decoder = decoders[0];
    now = 15;
    decoder.callbacks.output({ timestamp: decoder.chunks[0].timestamp });
    now = 5000;
    // The sender resumes with dependent pictures, not a fresh IDR. Delayed
    // decoder output after resume must not cause those pictures to be dropped.
    for (let i = 0; i < 8; i++) { player.decode(delta, now * 1000); now += 33; }
    assert.equal(decoder.chunks.length, 9);
    assert.equal(requests, 0);
    assert.equal(decoders.length, 1);
    decoder.callbacks.output({ timestamp: decoder.chunks[8].timestamp });
    assert.equal(player.takeDiagnostics().discarded, 0);
    // Continuous input without output still exercises genuine recovery.
    for (let i = 0; i < 20; i++) { now += 100; player.decode(delta, now * 1000); }
    assert.equal(requests, 1);
    assert.equal(player.takeDiagnostics().waitingKey, true);
  } finally {
    player.reset();
    globalThis.performance = oldPerformance;
    delete globalThis.VideoDecoder;
    delete globalThis.EncodedVideoChunk;
  }
});

test('H.264 first-picture watchdog does not request an IDR from an idle sender', () => {
  const oldPerformance = globalThis.performance, oldSetTimeout = globalThis.setTimeout, oldClearTimeout = globalThis.clearTimeout;
  let now = 10, watchdog;
  globalThis.performance = { now: () => now };
  globalThis.setTimeout = callback => { watchdog = callback; return 1; };
  globalThis.clearTimeout = () => {};
  globalThis.EncodedVideoChunk = class { constructor(options) { Object.assign(this, options); } };
  globalThis.VideoDecoder = class {
    constructor() { this.state = 'unconfigured'; this.decodeQueueSize = 0; this.chunks = []; }
    configure() { this.state = 'configured'; }
    decode(chunk) { this.chunks.push(chunk); }
    close() { this.state = 'closed'; }
  };
  let requests = 0;
  const player = new H264Player(() => {}, () => {}, () => requests++);
  try {
    player.decode(new Uint8Array([0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0, 0, 0, 1, 0x65, 0x80]), 10000);
    now = 3500; watchdog();
    player.decode(new Uint8Array([0, 0, 0, 1, 0x41, 0x80]), 3500000);
    assert.equal(player._decoder.chunks.length, 2);
    assert.equal(requests, 0);
    assert.equal(player.takeDiagnostics().waitingKey, false);
  } finally {
    player.reset(); globalThis.performance = oldPerformance;
    globalThis.setTimeout = oldSetTimeout; globalThis.clearTimeout = oldClearTimeout;
    delete globalThis.VideoDecoder; delete globalThis.EncodedVideoChunk;
  }
});
