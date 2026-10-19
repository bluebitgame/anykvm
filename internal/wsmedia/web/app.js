import { H264Player } from './h264.js';
import { H265Player } from './h265.js';
import { AV1Player } from './av1.js';
import { JPEGPlayer } from './jpeg.js';
import { TimestampFramePresenter } from './presenter.js';
import { parseTimedH264 } from './timed-video.js';
import { supportedAudioFormats, CompressedAudioPlayer } from './audio.js';
import { Keyboard } from './keyboard.js';
import { Pointer } from './pointer.js';
import { normalizeRotation, fitRotatedFrame } from './rotation.js';
import { ContainerPlayer } from './media.js';
import { bindFullscreen } from './fullscreen.js';

const MSG_META = 0x00, MSG_JPEG = 0x01, MSG_PCM = 0x02, MSG_ENCODED_AUDIO = 0x06, MSG_H264 = 0x10, MSG_HEVC = 0x11, MSG_AV1 = 0x12;

const canvas = document.getElementById('screen');
const wrap = document.getElementById('wrap');
const ctx = canvas.getContext('2d', { alpha: false, desynchronized: false });
const stat = document.getElementById('stat');
const audioBtn = document.getElementById('audioBtn');
const mediaVideo = document.getElementById('media');
const picker = document.getElementById('picker');
const viewer = document.getElementById('viewer');
const targetTitle = document.getElementById('targetTitle');

function targetNameFromPath() {
  const prefix = '/target/';
  if (!location.pathname.startsWith(prefix)) return '';
  try { return decodeURIComponent(location.pathname.slice(prefix.length)); }
  catch (_) { return ''; }
}
const selectedTarget = targetNameFromPath();

let meta = { w: 640, h: 480, sampleRate: 48000, channels: 2 };
let audioCtx = null, playHead = 0;
let audioEnabled = true, audioAvailable = false;
let frameCount = 0;
let videoMode = '—';
let videoNotice = '';
let receivedVideo = false;
let ws = null;
let videoRotation = 0;
let mediaWasActive = false;
const mediaPlayer = new ContainerPlayer(mediaVideo, {
  onChange: () => {
    canvas.hidden = mediaPlayer.active;
    if (mediaWasActive !== mediaPlayer.active) {
      mediaWasActive = mediaPlayer.active;
      videoNotice = '';
      h264Presenter.reset(); resetAudio();
      jpeg.reset(); h264.reset(); h265.reset(); av1.reset();
    }
    updateAudioButton();
  },
  onNotice: notice => { videoNotice = notice; },
});
window.addEventListener('pagehide', () => mediaPlayer.stop());

// --- codec selection (URL param overrides auto-detect) ---
const urlP = new URLSearchParams(location.search);
const forcedCodec = urlP.get('codec'); // "av1", "hevc", "h264", or "jpeg"
const debugVideo = urlP.get('debug') === 'video';
let diagnosticStart = performance.now();
let receivedFrames = 0, receivedVideoBytes = 0, drawnFrames = 0, drawMs = 0, drawMaxMs = 0;
let diagnosticText = '';
let statusStart = performance.now();
if (forcedCodec) console.log(`codec forced to: ${forcedCodec} (via ?codec=)`);

const hevcSupported = await H265Player.supported();
const h264Supported = await H264Player.supported();
const av1Supported = await AV1Player.supported();
let audioFormats = await supportedAudioFormats();
let audioCodecs = [...new Set(audioFormats.map(f => f.codec))];
let av1Failed = false;
let hevcFailed = false, h264Failed = false;

function codecCap() {
  // Explicit direct HEVC stays direct, including after decoder errors/reconnects.
  if (forcedCodec === 'hevc') return { h264: false, hevc: hevcSupported, av1: false };
  if (forcedCodec === 'h264') return { h264: h264Supported, hevc: false, av1: false };
  const failedForced = forcedCodec === 'av1' && av1Failed;
  const auto = !forcedCodec || failedForced;
  return {
    h264: !h264Failed && h264Supported && (auto || forcedCodec === 'h264'),
    hevc: !hevcFailed && hevcSupported && (auto || forcedCodec === 'hevc'),
    av1: !av1Failed && av1Supported && (auto || forcedCodec === 'av1'),
  };
}

const h264ok = codecCap().h264;
const hevcOk = codecCap().hevc;
if (!h264ok && !hevcOk && !codecCap().av1 && forcedCodec !== 'jpeg') {
  console.warn('No compatible WebCodecs decoder. Falling back to MJPEG; use HTTPS/localhost and a supported browser/OS.');
}

const size = () => ({ w: canvas.width, h: canvas.height, rotation: videoRotation });
const send = (bytes) => { if (ws && ws.readyState === 1) ws.send(bytes); };

// --- video ---
let presenterCodec = 'h264';
function presentVideoFrame(frame, codec, timestamp = frame.timestamp) {
  if (presenterCodec !== codec) {
    h264Presenter.reset();
    presenterCodec = codec;
  }
  h264Presenter.push(frame, timestamp);
}
const h264Presenter = new TimestampFramePresenter((frame) => {
  const start = debugVideo ? performance.now() : 0;
  videoNotice = '';
  resize(frame.displayWidth || frame.codedWidth, frame.displayHeight || frame.codedHeight);
  ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
  frameCount++;
  videoMode = presenterCodec;
  if (debugVideo) {
    const elapsed = performance.now() - start;
    drawnFrames++; drawMs += elapsed; drawMaxMs = Math.max(drawMaxMs, elapsed);
  }
});
window.addEventListener('pagehide', () => h264Presenter.reset());
const h264 = new H264Player((frame, presentationTime) => presentVideoFrame(frame, 'h264', presentationTime), (reason) => {
  h264Presenter.reset();
  if (h264Failed) return;
  h264Failed = true;
  videoNotice = `H.264 decoder: ${reason || 'unavailable'}`;
  if (forcedCodec !== 'h264') sendCapabilities();
}, () => {
  videoNotice = 'H.264: requesting a keyframe';
  send(new Uint8Array([0x85]));
});

const h265 = new H265Player((frame) => presentVideoFrame(frame, 'hevc'), (reason) => {
  h264Presenter.reset();
  if (hevcFailed) return;
  hevcFailed = true;
  videoNotice = `HEVC decoder: ${reason || 'unavailable'}`;
  if (forcedCodec !== 'hevc') sendCapabilities();
}, () => {
  videoNotice = 'HEVC: requesting a keyframe';
  send(new Uint8Array([0x85]));
});

const av1 = new AV1Player((frame) => presentVideoFrame(frame, 'av1'), () => {
  h264Presenter.reset();
  if (av1Failed) return;
  av1Failed = true;
  sendCapabilities();
});

function sendCapabilities() {
  const json = new TextEncoder().encode(JSON.stringify({ ...codecCap(), audioCodecs, audioFormats, rawPointer: pointer.rawMotionSupported }));
  const hello = new Uint8Array(1 + json.length);
  hello[0] = 0x83;
  hello.set(json, 1);
  send(hello);
}

function resize(w, h) {
  if (w && h && (w !== canvas.width || h !== canvas.height)) {
    canvas.width = w; canvas.height = h;
    layoutScreen();
  }
}

function layoutScreen() {
  const rect = wrap.getBoundingClientRect();
  if (rect.width <= 0 || rect.height <= 0) return;
  const fit = fitRotatedFrame(canvas.width, canvas.height, videoRotation, rect.width, rect.height);
  canvas.style.width = `${fit.width}px`;
  canvas.style.height = `${fit.height}px`;
  canvas.style.maxWidth = 'none';
  canvas.style.maxHeight = 'none';
  canvas.style.transform = `rotate(${videoRotation}deg)`;
}

window.addEventListener('resize', layoutScreen);
if (typeof ResizeObserver === 'function') new ResizeObserver(layoutScreen).observe(wrap);
layoutScreen();

const jpeg = new JPEGPlayer((bmp) => {
    h264Presenter.reset();
    resize(bmp.width, bmp.height);
    ctx.drawImage(bmp, 0, 0);
    frameCount++;
    videoMode = 'mjpeg';
});

// --- audio ---
const playingAudio = new Set();
function resetAudio() {
  compressedAudio.reset(); playHead = 0;
  for (const source of playingAudio) source.stop();
  playingAudio.clear();
}
const compressedAudio = new CompressedAudioPlayer(() => audioEnabled && audioAvailable ? audioCtx : null, playAudioBuffer, reason => {
  console.warn(`Compressed audio unavailable; requesting PCM: ${reason}`);
  audioCodecs = []; audioFormats = []; sendCapabilities();
});
window.addEventListener('pagehide', resetAudio);
function updateAudioButton() {
  const available = mediaPlayer.active || audioAvailable;
  const playing = available && (mediaPlayer.active ? !mediaVideo.muted : audioEnabled);
  audioBtn.textContent = !available ? '🔈' : playing ? '🔊' : '🔇';
  audioBtn.title = !available ? 'Audio unavailable' : playing ? 'Audio on (click to mute)' : 'Audio muted (click to enable)';
  audioBtn.disabled = !available;
  audioBtn.setAttribute('aria-label', audioBtn.title);
  audioBtn.setAttribute('aria-pressed', String(playing));
}
function startAudio() {
  if (mediaPlayer.active) return;
  if (!audioAvailable || !audioEnabled) return;
  try {
    if (!audioCtx) {
      audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: meta.sampleRate });
      audioCtx.addEventListener('statechange', updateAudioButton);
    }
    // Do not await: autoplay restrictions may defer this until a user gesture.
    audioCtx.resume().catch(e => console.warn(`Audio playback unavailable: ${e.message}`));
  } catch (e) { audioAvailable = false; updateAudioButton(); console.warn(`Audio playback unavailable: ${e.message}`); }
}
document.addEventListener('click', startAudio);
document.addEventListener('touchstart', startAudio, { passive: true });
audioBtn.onclick = async () => {
  if (mediaPlayer.active) { mediaPlayer.setMuted(!mediaVideo.muted); return; }
  if (!audioAvailable) return;
  audioEnabled = !audioEnabled;
  audioBtn.disabled = true;
  try {
    resetAudio();
    if (audioEnabled) startAudio();
    else if (audioCtx) await audioCtx.suspend();
  } catch (e) { console.warn(`Audio playback unavailable: ${e.message}`); }
  finally { updateAudioButton(); }
};
updateAudioButton();
function receivedAudio() {
  // A packet also confirms readiness if a metadata update was dropped.
  if (!audioAvailable) { audioAvailable = true; updateAudioButton(); startAudio(); }
}

function playPCM(bytes) {
  if (!audioEnabled || !audioAvailable || !audioCtx || audioCtx.state !== 'running') return;
  const ch = meta.channels || 2;
  const frames = Math.floor((bytes.length / 2) / ch);
  if (frames === 0) return;
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const abuf = audioCtx.createBuffer(ch, frames, meta.sampleRate || 48000);
  for (let c = 0; c < ch; c++) {
    const out = abuf.getChannelData(c);
    for (let i = 0; i < frames; i++) {
      const s = dv.getInt16((i * ch + c) * 2, true);
      out[i] = s < 0 ? s / 32768 : s / 32767;
    }
  }
  playAudioBuffer(abuf);
}

function playAudioBuffer(abuf, gain = 1) {
  if (!audioEnabled || !audioAvailable || !audioCtx || audioCtx.state !== 'running') return;
  const now = audioCtx.currentTime;
  if (playHead > now + 0.25) {
    for (const source of playingAudio) source.stop();
    playingAudio.clear(); playHead = 0;
  }
  const src = audioCtx.createBufferSource();
  src.buffer = abuf;
  let volume = null;
  if (gain === 1) src.connect(audioCtx.destination);
  else {
    volume = audioCtx.createGain(); volume.gain.value = gain;
    src.connect(volume); volume.connect(audioCtx.destination);
  }
  if (playHead < now) playHead = now + 0.05;
  playingAudio.add(src); src.onended = () => { playingAudio.delete(src); src.disconnect(); volume?.disconnect(); };
  src.start(playHead);
  playHead += abuf.duration;
}

// --- input ---
const keyboard = new Keyboard(canvas, send);
const pointer = new Pointer(canvas, send, size);

canvas.addEventListener('focus', () => { keyboard.grab(); });
canvas.addEventListener('blur', () => { keyboard.release(); });
bindFullscreen(document.getElementById('fullscreenBtn'), document.getElementById('fullscreenNotice'), (pageFullscreen, anyFullscreen) => {
  keyboard.releaseAll();
  pointer.releaseAll();
  layoutScreen();
  if (!mediaPlayer.active && (pageFullscreen || !anyFullscreen)) canvas.focus();
});
{ let done = false; document.addEventListener('click', () => { if (!done) { done = true; canvas.focus(); } }, { once: true }); }
{ let done = false; document.addEventListener('touchstart', () => { if (!done) { done = true; canvas.focus(); } }, { once: true }); }

// --- status ---
setInterval(() => {
  if (mediaPlayer.active) {
    const position = Number.isFinite(mediaVideo.currentTime) ? mediaVideo.currentTime.toFixed(1) : '0';
    stat.textContent = `AirPlay media — ${position}s${videoNotice ? ` — ${videoNotice}` : ''}`;
    frameCount = 0; statusStart = performance.now();
    return;
  }
  const now = performance.now();
  const fps = Math.round(frameCount * 1000 / (now - statusStart));
  let s = `${canvas.width}x${canvas.height} @ ${fps}fps [${videoMode}]`;
  if (receivedVideo && videoMode === '—') s += ' — receiving video, waiting for decoder';
  if (videoNotice) s += ` — ${videoNotice}`;
  if (!window.isSecureContext) s += ' — WebCodecs needs HTTPS/localhost';
  frameCount = 0;
  statusStart = now;
  if (debugVideo && now - diagnosticStart >= 5000) {
    const seconds = (now - diagnosticStart) / 1000;
    const decoder = videoMode === 'h264' || forcedCodec === 'h264' ? h264.takeDiagnostics() : h265.takeDiagnostics();
    const rate = count => Math.round(count / seconds * 10) / 10;
    const report = {
      receivedFps: rate(receivedFrames), decodedFps: rate(decoder.output), drawnFps: rate(drawnFrames),
      receivedMbps: Math.round(receivedVideoBytes * 8 / seconds / 1e6 * 100) / 100,
      drawAvgMs: drawnFrames ? Math.round(drawMs / drawnFrames * 10) / 10 : 0,
      drawMaxMs: Math.round(drawMaxMs * 10) / 10,
      ...decoder, visible: document.visibilityState, secure: window.isSecureContext,
      ...h264Presenter.takeDiagnostics(),
    };
    report.refreshFps = rate(report.refreshCallbacks);
    console.info('AnyKVM video diagnostics', JSON.stringify(report));
    const diagnostics = new TextEncoder().encode(JSON.stringify(report));
    const message = new Uint8Array(1 + diagnostics.length);
    message[0] = 0x86; message.set(diagnostics, 1); send(message);
    const details = [
      `fps recv/dec/draw/rAF ${report.receivedFps}/${report.decodedFps}/${report.drawnFps}/${report.refreshFps}`,
      `${report.receivedMbps}Mbps`,
      `decode ${decoder.decodeAvgMs ?? '—'}ms`,
      `gap max in/out/draw ${decoder.inputGapMaxMs ?? '—'}/${decoder.outputGapMaxMs ?? '—'}/${report.presentationDrawGapMaxMs}ms`,
      `buffer ${report.presentationQueue}/${report.presentationQueueCapacity}, wait max ${report.presentationWaitMaxMs}ms`,
      `display drop ${report.presentationSuperseded}`,
      `Δ(draw−PTS) abs/max ${report.presentationIntervalErrorAbsMeanMs ?? '—'}/${report.presentationIntervalErrorAbsMaxMs ?? '—'}ms`,
    ];
    if (report.presentationSuperseded) details.push(`overflow/catch-up ${report.presentationOverflowDrops}/${report.presentationCatchUpDrops}`);
    if (decoder.discarded) details.push(`decode drop ${decoder.discarded}`);
    if (decoder.keyRequests) details.push(`recovery ${decoder.keyRequests}`);
    if (report.visible !== 'visible') details.push(report.visible);
    diagnosticText = ` — ${details.join('; ')}`;
    receivedFrames = receivedVideoBytes = drawnFrames = drawMs = drawMaxMs = 0;
    diagnosticStart = now;
  }
  stat.textContent = s + diagnosticText;
}, 1000);

// --- connection ---
function connect() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/ws?target=${encodeURIComponent(selectedTarget)}`);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    stat.textContent = 'connected';
    sendCapabilities();
  };
  ws.onclose = () => { mediaPlayer.stop(); audioAvailable = false; updateAudioButton(); resetAudio(); h264Presenter.reset(); jpeg.reset(); h264.reset(); h265.reset(); av1.reset(); stat.textContent = 'disconnected — retrying…'; keyboard.releaseAll(); pointer.releaseAll(); setTimeout(connect, 1000); };
  ws.onerror = () => ws.close();

  ws.onmessage = (ev) => {
    const buf = new Uint8Array(ev.data);
    const type = buf[0];
    const payload = buf.subarray(1);
    if (mediaPlayer.active && type !== MSG_META) return;
    switch (type) {
      case MSG_META:
        try {
          const previousW = meta.w, previousH = meta.h;
          meta = JSON.parse(new TextDecoder().decode(payload));
          if (meta.mouseMode !== undefined) pointer.setMode(meta.mouseMode);
          if (meta.audioAvailable !== undefined) {
            audioAvailable = meta.audioAvailable;
            if (!audioAvailable) resetAudio();
            updateAudioButton(); startAudio();
          }
          if (meta.videoBufferFrames !== undefined) h264Presenter.setQueueCapacity(meta.videoBufferFrames);
          const rotation = normalizeRotation(meta.rotation);
          if (rotation !== videoRotation) {
            pointer.releaseAll();
            videoRotation = rotation;
            layoutScreen();
          }
          if (meta.w && meta.h) {
            if (meta.w !== previousW || meta.h !== previousH) h264Presenter.reset();
            h264.setSize(meta.w, meta.h);
            h265.setSize(meta.w, meta.h);
            av1.setSize(meta.w, meta.h);
          }
        } catch (e) {}
        break;
      case MSG_JPEG:
        if (forcedCodec === 'h264') { videoNotice = 'H.264 requested; server sent JPEG — check server capture errors'; break; }
        receivedVideo = true; jpeg.push(payload); break;
      case MSG_PCM: receivedAudio(); compressedAudio.reset(); playPCM(payload); break;
      case MSG_ENCODED_AUDIO: receivedAudio(); compressedAudio.push(payload); break;
      case MSG_H264: {
        const picture = parseTimedH264(payload);
        if (!picture) { videoNotice = 'Invalid H.264 timestamp message'; break; }
        receivedFrames++; receivedVideoBytes += picture.data.byteLength;
        receivedVideo = true; jpeg.reset(); h264.decode(picture.data, picture.timestamp); break;
      }
      case MSG_HEVC:
        if (forcedCodec === 'h264') { videoNotice = 'H.264 requested; server sent HEVC'; break; }
        receivedFrames++; receivedVideoBytes += payload.byteLength; receivedVideo = true; jpeg.reset(); h265.decode(payload); break;
      case MSG_AV1: receivedVideo = true; jpeg.reset(); av1.decode(payload); break;
    }
  };
}

let pollingMedia = false;
setInterval(async () => {
  if (pollingMedia || !ws || ws.readyState !== 1) return;
  pollingMedia = true;
  const connection = ws;
  try {
    const response = await fetch('/airplay/media-state', { cache: 'no-store' });
    if (!response.ok) return;
    const state = await response.json();
    if (ws === connection && ws.readyState === 1) await mediaPlayer.update(state);
  } catch (_) {} finally { pollingMedia = false; }
}, 500);

async function showTargetPicker() {
  picker.hidden = false;
  viewer.hidden = true;
  const grid = document.getElementById('targetGrid');
  const empty = document.getElementById('targetEmpty');
  const settings = document.getElementById('settingsLink');
  try {
    const response = await fetch('/api/targets', { cache: 'no-store' });
    if (!response.ok) throw new Error(await response.text());
    const payload = await response.json();
    settings.hidden = !payload.settings;
    empty.hidden = payload.targets.length !== 0;
    for (const target of payload.targets) {
      const link = document.createElement('a');
      link.className = 'targetCard';
      link.href = `/target/${encodeURIComponent(target.name)}`;
      const cover = document.createElement('img');
      cover.src = target.cover;
      cover.alt = '';
      const name = document.createElement('span');
      name.textContent = target.name;
      link.append(cover, name);
      grid.append(link);
    }
  } catch (error) {
    empty.hidden = false;
    empty.textContent = `Unable to load targets: ${error.message}`;
  }
}

function showTargetControl() {
  picker.hidden = true;
  viewer.hidden = false;
  targetTitle.textContent = selectedTarget;
  layoutScreen();
  if (forcedCodec === 'h264' && !h264Supported) {
    videoNotice = 'H.264 WebCodecs unavailable — use localhost/HTTPS and a supported browser';
    stat.textContent = videoNotice;
    return;
  }
  connect();
}

if (selectedTarget) showTargetControl();
else showTargetPicker();
