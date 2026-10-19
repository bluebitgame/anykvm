import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.ANYKVM_TEST_PLAYWRIGHT);
const [web, airplay, cdn] = process.argv.slice(2);
const browser = await chromium.launch({ executablePath: process.env.ANYKVM_TEST_BROWSER_CHROME, headless: true });
try {
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (message.type() === 'error') console.log('browser:', message.text()); });
  await page.goto(web);
  const control = async (path, body) => {
    const response = await fetch(airplay + path, { method: 'POST', headers: { 'X-Apple-Session-ID': 'browser', 'Content-Type': 'text/parameters' }, body });
    assert.equal(response.status, 200, `${path}: ${response.status}`);
  };
  for (const [name, location] of [
    ['MP4', cdn + '/movie.mp4'],
    ['MPEG-TS HLS', cdn + '/movie.m3u8'],
    ['fMP4 HLS', cdn + '/fragmented.m3u8'],
    ['FCUP fMP4 HLS', 'mlhls://localhost/fragmented.m3u8'],
  ]) {
    await control('/play', `Content-Location: ${location}\nStart-Position-Seconds: 0.25`);
    try { await page.waitForFunction(() => {
      const v = document.getElementById('media');
      return !v.paused && v.currentTime > .45 && v.videoWidth === 64 && v.videoHeight === 48 && v.getVideoPlaybackQuality().totalVideoFrames > 0;
    }, null, { timeout: 10000 }); } catch(error) {
      console.log('playback diagnostics', await page.evaluate(() => {
        const v=document.getElementById('media');
        return {src:v.currentSrc,error:v.error?.message,ready:v.readyState,network:v.networkState,time:v.currentTime,paused:v.paused,muted:v.muted,buffered:Array.from({length:v.buffered.length},(_,i)=>[v.buffered.start(i),v.buffered.end(i)]),size:[v.videoWidth,v.videoHeight],frames:v.getVideoPlaybackQuality().totalVideoFrames,notice:document.getElementById('stat').textContent};
      }));
      throw error;
    }
    await control('/rate?value=0');
    await page.waitForFunction(() => document.getElementById('media').paused);
    const before = await page.$eval('#media', v => v.currentTime);
    await page.waitForTimeout(150);
    const after = await page.$eval('#media', v => v.currentTime);
    assert.ok(Math.abs(after - before) < .05, `${name} failed pause`);
    await control('/scrub?position=1.1');
    await page.waitForFunction(() => {
      const v = document.getElementById('media');
      return v.paused && Math.abs(v.currentTime - 1.1) < .05;
    });
    await control('/rate?value=1');
    await page.waitForFunction(() => document.getElementById('media').currentTime > 1.5);
    const quality = await page.$eval('#media', v => ({ frames: v.getVideoPlaybackQuality().totalVideoFrames, decodedAudioBytes: v.webkitAudioDecodedByteCount }));
    console.log(name, JSON.stringify(quality));
    await control('/stop');
    await page.waitForFunction(() => document.getElementById('media').hidden);
  }
  assert.deepEqual(errors, []);
} finally { await browser.close(); }
