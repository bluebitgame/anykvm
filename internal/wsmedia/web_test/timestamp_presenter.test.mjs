import test from 'node:test';
import assert from 'node:assert/strict';
import { TimestampFramePresenter } from '../web/presenter.js';
import { parseTimedH264 } from '../web/timed-video.js';

function fixture(draw, bufferMs = 1000 / 60) {
  let now = 100, next = 0;
  const scheduled = new Map();
  const player = new TimestampFramePresenter(draw, {
    now: () => now,
    bufferMs, queueFrames: bufferMs > 0 ? 3 : 1,
    schedule: callback => { const handle = next++; scheduled.set(handle, callback); return handle; },
    cancel: handle => scheduled.delete(handle),
  });
  const picture = id => ({ id, closed: 0, close() { this.closed++; } });
  const refresh = at => { now = at; const [handle, callback] = scheduled.entries().next().value; scheduled.delete(handle); callback(); };
  return { player, picture, refresh, scheduled, at: value => { now = value; } };
}

test('default low-latency mode draws the latest picture without a PTS wait or burst queue', () => {
  const drawn = [];
  // Exercise zero buffering and separately verify the production default.
  const { player, picture, refresh } = fixture(frame => drawn.push(frame.id), 0);
  assert.equal(new TimestampFramePresenter(() => {}).bufferMs, 0);
  const frames = [picture(1), picture(2), picture(3)];
  frames.forEach((frame, i) => player.push(frame, 1000000 + i * 16667));
  refresh(101);
  assert.deepEqual(drawn, [3]);
  assert.ok(frames.every(frame => frame.closed === 1));
  assert.equal(player.takeDiagnostics().presentationTargetBufferMs, 0);
  player.reset();
});

test('draw versus PTS statistics preserve signed error and jitter across reporting windows', () => {
  const { player, picture, refresh, at } = fixture(() => {}, 0);
  assert.equal(player.takeDiagnostics().presentationIntervalErrorMeanMs, null);
  for (const [id, time, pts] of [[0, 100, 1000000], [1, 120, 1010000], [2, 130, 1030000], [3, 160, 1050000]]) {
    at(time); player.push(picture(id), pts); refresh(time);
  }
  const stats = player.takeDiagnostics();
  assert.equal(stats.presentationIntervalSamples, 3);
  assert.equal(stats.presentationIntervalErrorMeanMs, 3.3);
  assert.equal(stats.presentationIntervalErrorAbsMeanMs, 10);
  assert.equal(stats.presentationIntervalErrorAbsMaxMs, 10);
  assert.equal(player.takeDiagnostics().presentationIntervalSamples, 0);
  // Reporting resets aggregates, not the preceding displayed frame.
  at(190); player.push(picture(4), 1090000); refresh(190);
  const next = player.takeDiagnostics();
  assert.equal(next.presentationIntervalSamples, 1);
  assert.equal(next.presentationIntervalErrorMeanMs, -10);
  assert.equal(next.presentationIntervalErrorAbsMeanMs, 10);
  player.reset();
});

test('interval comparison uses the actual displayed PTS after skips and excludes source resets', () => {
  const { player, picture, refresh, at } = fixture(() => {}, 0);
  player.push(picture(0), 1000000); refresh(100);
  at(110); player.push(picture(1), 1010000); player.push(picture(2), 1030000);
  refresh(120);
  const stats = player.takeDiagnostics();
  assert.equal(stats.presentationIntervalSamples, 1);
  assert.equal(stats.presentationIntervalErrorMeanMs, -10);
  at(130); player.push(picture(3), 100000); refresh(130);
  assert.equal(player.takeDiagnostics().presentationIntervalSamples, 0);
  player.reset();
  at(200); player.push(picture(4), 200000); refresh(200);
  assert.equal(player.takeDiagnostics().presentationIntervalSamples, 0);
  player.reset();
});

test('failed draws and long background gaps do not become interval error samples', () => {
  let fail = false;
  const { player, picture, refresh, at } = fixture(() => { if (fail) throw new Error('draw failed'); }, 0);
  player.push(picture(0), 1000000); refresh(100);
  fail = true;
  at(120); player.push(picture(1), 1020000);
  assert.throws(() => refresh(120), /draw failed/);
  fail = false;
  at(140); player.push(picture(2), 1040000); refresh(140);
  const stats = player.takeDiagnostics();
  assert.equal(stats.presentationIntervalSamples, 1);
  assert.equal(stats.presentationIntervalErrorAbsMaxMs, 0);
  at(150); player.push(picture(3), 1060000); refresh(3000);
  assert.equal(player.takeDiagnostics().presentationIntervalSamples, 0);
  player.reset();
});

test('configured eight-frame queue preserves eight-picture bursts and bounds overflow', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id), 0);
  player.setQueueCapacity(8);
  const frames = Array.from({ length: 8 }, (_, i) => picture(i));
  frames.forEach((frame, i) => player.push(frame, 1000000 + Math.round(i * 1000000 / 60)));
  for (let i = 1; i <= 8; i++) refresh(100 + i * 1000 / 60);
  assert.deepEqual(drawn, frames.map(frame => frame.id));
  assert.ok(frames.every(frame => frame.closed === 1));
  const stats = player.takeDiagnostics();
  assert.equal(stats.presentationQueueCapacity, 8);
  assert.equal(stats.presentationSuperseded, 0);
  at(300);
  const burst = Array.from({ length: 12 }, (_, i) => picture(i + 8));
  burst.forEach((frame, i) => player.push(frame, 1200000 + i * 16667));
  assert.equal(player.queue.length, 8);
  assert.ok(burst.slice(0, 4).every(frame => frame.closed === 1));
  player.setQueueCapacity(1);
  assert.ok(burst.every(frame => frame.closed === 1));
  player.push(picture(20), 1500000); refresh(301);
  assert.equal(drawn.at(-1), 20);
  player.reset();
});

test('source policy changes clear pending pictures and default back to immediate presentation', () => {
  const drawn = [];
  const { player, picture, refresh } = fixture(frame => drawn.push(frame.id), 0);
  player.setQueueCapacity(3);
  const first = picture(1); player.push(first, 1000000);
  refresh(101); assert.deepEqual(drawn, []);
  player.setQueueCapacity(3); // Repeated geometry metadata must not discard pictures.
  assert.equal(first.closed, 0);
  refresh(117); assert.deepEqual(drawn, [1]);
  const pending = picture(2); player.push(pending, 1033333);
  player.setQueueCapacity(1);
  assert.equal(pending.closed, 1);
  player.push(picture(3), 1066666);
  refresh(118); assert.deepEqual(drawn, [1, 3]);
  assert.equal(player.takeDiagnostics().presentationMode, 'latest-frame-vsync');
  player.reset();
});

test('short decoded bursts drain at successive refreshes even without new input', () => {
  const drawn = [];
  const { player, picture, refresh, scheduled } = fixture(frame => drawn.push(frame.id));
  const frames = [picture(1), picture(2), picture(3)];
  frames.forEach((frame, i) => player.push(frame, 1000000 + i * 16667));
  refresh(117); refresh(134); refresh(150);
  assert.deepEqual(drawn, [1, 2, 3]);
  assert.ok(frames.every(frame => frame.closed === 1));
  refresh(166); assert.deepEqual(drawn, [1, 2, 3]);
  const stats = player.takeDiagnostics();
  assert.equal(stats.presentationQueue, 0);
  assert.equal(stats.presentationTargetBufferMs, 16.7);
  assert.equal(stats.refreshCallbacks, 4);
  assert.equal(scheduled.size, 1); // Measure refreshes independently of input FPS.
  player.reset(); assert.equal(scheduled.size, 0);
});

test('steady 60fps source timing survives microsecond rounding without skipped refreshes', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  for (let i = 0; i < 120; i++) {
    at(100 + i * 1000 / 60);
    player.push(picture(i), 1000000 + Math.round(i * 1000000 / 60));
    refresh(100 + (i + 1) * 1000 / 60);
    assert.equal(drawn.length, i + 1);
  }
  const stats = player.takeDiagnostics();
  assert.equal(stats.presentationRebases, 0);
  assert.equal(stats.presentationBufferedMs, 0);
  assert.equal(stats.refreshCallbacks, 120);
});

test('two-frame bursts on a 60Hz display draw every source picture', () => {
  const drawn = [], frames = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  for (let pair = 0; pair < 60; pair++) {
    at(100 + pair * 1000 / 30);
    for (let n = 0; n < 2; n++) {
      const id = pair * 2 + n, frame = picture(id);
      frames.push(frame); player.push(frame, 1000000 + Math.round(id * 1000000 / 60));
    }
    refresh(100 + (pair * 2 + 1) * 1000 / 60);
    refresh(100 + (pair * 2 + 2) * 1000 / 60);
  }
  assert.deepEqual(drawn, Array.from({ length: 120 }, (_, i) => i));
  assert.ok(frames.every(frame => frame.closed === 1));
  assert.equal(player.takeDiagnostics().presentationSuperseded, 0);
});

for (const [sourceHz, displayHz] of [[60, 120], [30, 60]]) {
  test(`${sourceHz}fps paired bursts keep source spacing on a ${displayHz}Hz display`, () => {
    const draws = [], frames = [];
    let tick = 0;
    const { player, picture, refresh, at } = fixture(frame => draws.push({ id: frame.id, tick }));
    for (let pair = 0; pair < 60; pair++) {
      at(100 + pair * 2000 / sourceHz);
      for (let n = 0; n < 2; n++) {
        const id = pair * 2 + n, frame = picture(id);
        frames.push(frame);
        player.push(frame, 1000000 + Math.round(id * 1000000 / sourceHz));
      }
      for (let n = 0; n < 4; n++) {
        tick++;
        refresh(100 + tick * 1000 / displayHz);
      }
    }
    assert.deepEqual(draws.map(draw => draw.id), frames.map(frame => frame.id));
    // FIFO-per-refresh would alternate 1 and 3 ticks instead of a steady 2.
    assert.ok(draws.slice(1).every((draw, i) => draw.tick - draws[i].tick === 2));
    assert.ok(frames.every(frame => frame.closed === 1));
    assert.equal(player.takeDiagnostics().presentationRebases, 0);
    player.reset();
  });
}

test('arrival jitter does not move the presentation clock', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  for (let i = 0; i < 120; i++) {
    at(100 + i * 1000 / 60 + (i % 3) * 3);
    player.push(picture(i), 1000000 + Math.round(i * 1000000 / 60));
    refresh(100 + (i + 1) * 1000 / 60);
    assert.equal(drawn.length, i + 1);
  }
  assert.equal(player.takeDiagnostics().presentationRebases, 0);
  player.reset();
});

test('a source timestamp offset does not stall a continuously decoded 60fps stream', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  player.setQueueCapacity(8);
  for (let i = 0; i < 180; i++) {
    at(100 + i * 1000 / 60);
    const offset = i >= 60 ? 80000 : 0;
    player.push(picture(i), 1000000 + Math.round(i * 1000000 / 60) + offset);
    refresh(100 + (i + 1) * 1000 / 60);
    assert.equal(drawn.length, i + 1, `display stalled at frame ${i}`);
  }
  assert.equal(player.takeDiagnostics().presentationSuperseded, 0);
  player.reset();
});

test('slow source cadence cannot expire an eight-frame decoder burst in the display queue', () => {
  const drawn = [];
  const { player, picture, refresh } = fixture(frame => drawn.push(frame.id), 0);
  player.setQueueCapacity(8);
  const frames = Array.from({ length: 8 }, (_, i) => picture(i));
  frames.forEach((frame, i) => player.push(frame, 1000000 + i * 80000));
  for (let i = 1; i <= 8; i++) refresh(100 + i * 1000 / 60);
  assert.deepEqual(drawn, frames.map(frame => frame.id));
  assert.ok(frames.every(frame => frame.closed === 1));
  assert.equal(player.takeDiagnostics().presentationCatchUpDrops, 0);
  player.reset();
});

test('a new burst shortens an already pending slow cadence deadline', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id), 0);
  player.setQueueCapacity(8);
  player.push(picture(0), 1000000);
  refresh(117);
  at(120);
  for (let i = 1; i <= 8; i++) player.push(picture(i), 1000000 + i * 80000);
  for (let i = 1; i <= 8; i++) refresh(120 + i * 1000 / 60);
  assert.deepEqual(drawn, Array.from({length: 9}, (_, i) => i));
  assert.equal(player.takeDiagnostics().presentationCatchUpDrops, 0);
  player.reset();
});

test('starvation rebases once and preserves spacing in the resumed burst', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  player.push(picture(1), 1000000); refresh(117);
  at(600);
  for (let i = 0; i < 3; i++) player.push(picture(i + 2), 1016667 + i * 16667);
  refresh(608); assert.deepEqual(drawn, [1, 2]);
  refresh(617); refresh(634); refresh(650);
  assert.deepEqual(drawn, [1, 2, 3, 4]);
  at(651); player.push(picture(5), 1066668);
  refresh(667); assert.deepEqual(drawn, [1, 2, 3, 4, 5]);
  assert.equal(player.takeDiagnostics().presentationRebases, 1);
  player.reset();
});

test('catch-up selects a fresh picture without waiting for its absolute source timestamp', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  player.push(picture(1), 1000000);
  at(180); player.push(picture(2), 1113333);
  refresh(180); assert.deepEqual(drawn, [2]);
  refresh(220); assert.deepEqual(drawn, [2]);
  refresh(230); assert.deepEqual(drawn, [2]);
  player.reset();
});

test('late burst catch-up must not keep halving decoder output on subsequent bursts', () => {
  const drawn = [], frames = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  // A picture remains pending while delivery/decoding stalls for 400ms.
  // Later pairs retain their old source timeline, as in a delayed decoder.
  player.push(picture(-1), 1000000);
  for (let pair = 0; pair < 60; pair++) {
    const arrival = 500 + pair * 1000 / 30;
    at(arrival);
    for (let n = 0; n < 2; n++) {
      const id = pair * 2 + n, frame = picture(id);
      frames.push(frame);
      player.push(frame, 1016667 + Math.round(id * 1000000 / 60));
    }
    refresh(arrival + 1000 / 60);
    refresh(arrival + 2000 / 60);
  }
  // One initial catch-up is allowed, not one dropped picture in every pair.
  assert.ok(drawn.length >= 118, `only ${drawn.length} of 120 decoded pictures drawn`);
  assert.ok(frames.every(frame => frame.closed === 1));
  assert.equal(player.takeDiagnostics().presentationRebases, 1);
  player.reset();
});

test('fresh decoder bursts are not discarded just because source PTS is behind arrival', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  for (let burst = 0; burst < 60; burst++) {
    const arrival = 100 + burst * 80;
    at(arrival);
    for (let n = 0; n < 3; n++) {
      const id = burst * 3 + n;
      player.push(picture(id), 1000000 + Math.round(id * 1000000 / 60));
    }
    for (let n = 1; n <= 4; n++) refresh(arrival + n * 1000 / 60);
  }
  assert.equal(drawn.length, 180);
  assert.equal(player.takeDiagnostics().presentationSuperseded, 0);
  player.reset();
});

test('a 30Hz display draws on every callback without falling to 15fps or retaining unbounded history', () => {
  const drawn = [], frames = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  for (let pair = 0; pair < 60; pair++) {
    at(100 + pair * 1000 / 30);
    for (let n = 0; n < 2; n++) {
      const id = pair * 2 + n, frame = picture(id);
      frames.push(frame); player.push(frame, 1000000 + Math.round(id * 1000000 / 60));
    }
    refresh(100 + (pair + 1) * 1000 / 30);
    assert.equal(drawn.length, pair + 1);
    assert.ok(player.queue.length <= 2);
  }
  const stats = player.takeDiagnostics();
  assert.equal(stats.refreshCallbacks, 60);
  assert.ok(Math.abs(stats.refreshGapMaxMs - 33.3) < 0.1);
  player.reset();
  assert.ok(frames.every(frame => frame.closed === 1));
});

test('large output bursts retain at most three pictures and missed refreshes select the latest', () => {
  const drawn = [];
  const { player, picture, refresh } = fixture(frame => drawn.push(frame.id));
  const frames = Array.from({ length: 100 }, (_, i) => picture(i));
  frames.forEach((frame, i) => player.push(frame, 1000000 + i * 16667));
  const stats = player.takeDiagnostics();
  assert.equal(stats.presentationQueue, 3);
  assert.equal(stats.presentationQueueMax, 3);
  assert.equal(stats.decodedBurstMax, 100);
  assert.equal(stats.presentationOverflowDrops, 97);
  assert.equal(stats.presentationCatchUpDrops, 0);
  assert.ok(frames.slice(0, -3).every(frame => frame.closed === 1));
  refresh(250); assert.deepEqual(drawn, [99]);
  assert.equal(player.takeDiagnostics().presentationCatchUpDrops, 2);
  const nextWindow = player.takeDiagnostics();
  assert.equal(nextWindow.decodedBurstMax, 0);
  assert.equal(nextWindow.presentationOverflowDrops, 0);
  assert.equal(nextWindow.presentationCatchUpDrops, 0);
  assert.ok(frames.every(frame => frame.closed === 1));
});

test('delayed older output and repeated PTS cannot rewind or repeat the display', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  player.push(picture(1), 1000000); refresh(117);
  at(120); player.push(picture(3), 1033334);
  const older = picture(2); player.push(older, 1016667);
  refresh(134); refresh(150); assert.deepEqual(drawn, [1, 2, 3]);
  const stale = picture(0); player.push(stale, 1016667);
  assert.equal(stale.closed, 1);
  const a = picture(4), b = picture(4);
  player.push(a, 1050001); player.push(b, 1050001);
  refresh(167); refresh(183);
  assert.deepEqual(drawn, [1, 2, 3, 4]);
  assert.equal(a.closed, 1); assert.equal(b.closed, 1);
});

test('quiet resumes and source resets clear stale pictures and retain synchronization', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  const first = picture(1); player.push(first, 1000000);
  at(5100); player.push(picture(2), 6000000);
  assert.equal(first.closed, 1); refresh(5117);
  const old = picture(3); player.push(old, 6020000);
  player.push(picture(4), 100000);
  assert.equal(old.closed, 1); refresh(5134);
  assert.deepEqual(drawn, [2, 4]);
});

test('a long browser refresh pause is measured and the next callback displays pending output', () => {
  const drawn = [];
  const { player, picture, refresh, at } = fixture(frame => drawn.push(frame.id));
  player.push(picture(1), 1000000); refresh(117);
  at(120); player.push(picture(2), 1016667); player.push(picture(3), 1033334);
  refresh(41116);
  assert.deepEqual(drawn, [1, 3]);
  const stats = player.takeDiagnostics();
  assert.equal(stats.refreshGapMaxMs, 40999);
  assert.equal(stats.presentationQueue, 0);
  assert.equal(stats.presentationWaitMaxMs, 40996);
});

test('reset cancels refresh monitoring and releases pending pictures exactly once', () => {
  const { player, picture, scheduled } = fixture(() => {});
  const frame = picture(1); player.push(frame, 1000000);
  player.reset(); player.reset();
  assert.equal(frame.closed, 1); assert.equal(scheduled.size, 0);
});

test('draw failures release output and do not strand the remaining queue', () => {
  let failed = true;
  const { player, picture, refresh } = fixture(() => { if (failed) throw new Error('draw failed'); });
  const first = picture(1), next = picture(2);
  player.push(first, 1000000); player.push(next, 1016000);
  assert.throws(() => refresh(117), /draw failed/);
  assert.equal(first.closed, 1);
  failed = false; refresh(133); assert.equal(next.closed, 1);
});

test('timestamp payload parsing preserves exact microseconds and Annex-B bytes', () => {
  const source = 1791655600123456n;
  const storage = new Uint8Array(24);
  const payload = storage.subarray(3, 17);
  new DataView(payload.buffer, payload.byteOffset).setBigUint64(0, source, true);
  payload.set([0, 0, 0, 1, 0x65, 0x80], 8);
  const parsed = parseTimedH264(payload);
  assert.equal(parsed.timestamp, Number(source));
  assert.deepEqual([...parsed.data], [0, 0, 0, 1, 0x65, 0x80]);
  assert.equal(parsed.data.buffer, storage.buffer);
  assert.equal(parseTimedH264(payload.subarray(0, 8)), null);
  new DataView(payload.buffer, payload.byteOffset).setBigUint64(0, 0n, true);
  assert.equal(parseTimedH264(payload), null);
  new DataView(payload.buffer, payload.byteOffset).setBigUint64(0, 1n << 53n, true);
  assert.equal(parseTimedH264(payload), null);
});
