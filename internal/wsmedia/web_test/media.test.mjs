import test from 'node:test';
import assert from 'node:assert/strict';
import { ContainerPlayer } from '../web/media.js';

const id = '1'.repeat(32);
const initial = { id, url: `/airplay/media/${id}/resource`, version: 1, seekVersion: 1, paused: false, position: 0.25, startFraction: 0 };
function fixture(type = 'video/mp4') {
  const video = new EventTarget();
  Object.assign(video, {
    readyState: 0, currentTime: 0, duration: 2, muted: false, paused: true, ended: false, hidden: true,
    buffered: { length: 1, start: () => 0, end: () => 1.2 },
    seekable: { length: 1, start: () => 0, end: () => 2 },
    canPlayType: () => '', load() {}, removeAttribute(key) { delete this[key]; },
    async play() { this.paused = false; }, pause() { this.paused = true; },
  });
  const requests = [];
  const player = new ContainerPlayer(video, { fetcher: async (url, options) => {
    requests.push({ url, options });
    return { ok: true, headers: { get: () => type } };
  } });
  return { video, player, requests };
}

test('MP4 controls wait for metadata, do not rewind during polling, and report real ranges', async () => {
  const { player, video, requests } = fixture();
  await player.update(initial);
  assert.equal(video.src, initial.url);
  assert.equal(video.currentTime, 0);
  video.readyState = 4;
  video.dispatchEvent(new Event('loadedmetadata'));
  assert.equal(video.currentTime, .25);
  assert.equal(video.paused, false);
  video.currentTime = .8;
  await player.update(initial);
  assert.equal(video.currentTime, .8);
  await player.update({ ...initial, version: 2, paused: true, position: .4 });
  assert.equal(video.paused, true);
  assert.equal(video.currentTime, .8);
  await player.update({ ...initial, version: 3, seekVersion: 3, paused: true, position: 1.1 });
  assert.equal(video.currentTime, 1.1);
  player.report(true);
  const report = JSON.parse(requests.at(-1).options.body);
  assert.equal(report.version, 3);
  assert.equal(report.position, 1.1);
  assert.equal(report.state, 'paused');
  assert.deepEqual(report.buffered, [{ start: 0, duration: 1.2 }]);
  player.stop();
  assert.equal(video.hidden, true);
  assert.equal(video.src, undefined);
  assert.equal(player.active, false);
});

test('HLS fallback uses bundled transmuxer and releases it on stop', async t => {
  const { player, video } = fixture('application/vnd.apple.mpegurl');
  let destroyed = false, loaded, attached;
  globalThis.Hls = class {
    static isSupported() { return true; }
    static Events = { ERROR: 'error' };
    on() {}
    loadSource(url) { loaded = url; }
    attachMedia(media) { attached = media; }
    destroy() { destroyed = true; }
  };
  t.after(() => { delete globalThis.Hls; });
  await player.update(initial);
  assert.equal(loaded, initial.url);
  assert.equal(attached, video);
  player.stop();
  assert.equal(destroyed, true);
});

test('native HLS and fractional starting position bypass hls.js', async t => {
  const { player, video } = fixture('application/vnd.apple.mpegurl');
  globalThis.ManagedMediaSource = class {};
  t.after(() => { delete globalThis.ManagedMediaSource; });
  video.canPlayType = () => 'probably';
  await player.update({ ...initial, position: 0, startFraction: .5 });
  video.readyState = 4;
  video.dispatchEvent(new Event('loadedmetadata'));
  assert.equal(player.hls, null);
  assert.equal(video.currentTime, 1);
});

test('autoplay denial falls back to muted playback and user gesture enables audio', async () => {
  const { player, video } = fixture();
  video.play = async function () {
    if (!this.muted) throw Object.assign(new Error('gesture required'), { name: 'NotAllowedError' });
    this.paused = false;
  };
  await player.update(initial);
  video.readyState = 4;
  video.dispatchEvent(new Event('loadedmetadata'));
  await Promise.resolve();
  assert.equal(video.muted, true);
  assert.equal(video.paused, false);
  video.play = async function () { this.paused = false; };
  player.setMuted(false);
  assert.equal(video.muted, false);
});

test('stale resource probe cannot revive a stopped player', async () => {
  const { video } = fixture();
  let resolve;
  const player = new ContainerPlayer(video, { fetcher: () => new Promise(r => { resolve = r; }) });
  const pending = player.update(initial);
  player.stop();
  resolve({ ok: true, headers: { get: () => 'video/mp4' } });
  await pending;
  assert.equal(player.active, false);
  assert.equal(video.src, undefined);
});
