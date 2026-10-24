// H.264 decoder using the WebCodecs API.
// Each call to decode(au) receives a complete access unit (pre-parsed by the
// server's push-based Annex-B parser). No streaming — no partial buffering.

function startCodeLen(d, i) {
  if (d[i] === 0 && d[i + 1] === 0 && d[i + 2] === 0 && d[i + 3] === 1) return 4;
  if (d[i] === 0 && d[i + 1] === 0 && d[i + 2] === 1) return 3;
  return 0;
}

function nextStartCode(d, i) {
  for (let j = i; j + 2 < d.length; j++) {
    if (d[j] === 0 && d[j + 1] === 0 && d[j + 2] === 1) return j;
    if (d[j] === 0 && d[j + 1] === 0 && d[j + 2] === 0 && d[j + 3] === 1) return j;
  }
  return -1;
}

function scanAU(au) {
  let key = false;
  let sps = null;
  let i = 0;
  while (i < au.length) {
    const sc = startCodeLen(au, i);
    if (sc === 0) { i++; continue; }
    const type = au[i + sc] & 0x1f;
    if (type === 5) key = true;
    if (type === 7) sps = [au[i + sc + 1], au[i + sc + 2], au[i + sc + 3]];
    const next = nextStartCode(au, i + sc);
    i = next === -1 ? au.length : next;
  }
  return { key, sps };
}

function decodeStats() {
  return { received: 0, submitted: 0, output: 0, discarded: 0, keyRequests: 0, queueMax: 0, pendingMax: 0, unmatchedOutput: 0, decodeMs: 0, decodeMaxMs: 0, inputGapMaxMs: 0, outputGapMaxMs: 0 };
}

export class H264Player {
  constructor(onFrame, onUnsupported = () => {}, onKeyframeNeeded = () => {}) {
    this._onFrame = onFrame;
    this._onUnsupported = onUnsupported;
    this._onKeyframeNeeded = onKeyframeNeeded;
    this._decoder = null;
    this._codec = null;
    this._w = 0;
    this._h = 0;
    this._ts = 0;
    this._pressureAt = null;
    this._keyRequested = false;
    this._waitingKey = false;
    this._hasOutput = false;
    this._lastOutputAt = 0;
    this._lastInputAt = null;
    this._lastProgressAt = 0;
    this._restartPending = false;
    this._firstFrameTimer = null;
    this._failed = false;
    this._stats = decodeStats();
    this._submissions = new Map();
  }

  static async supported() {
    if (typeof VideoDecoder !== 'function') return false;
    try {
      return (await VideoDecoder.isConfigSupported({ codec: 'avc1.42e01f', optimizeForLatency: true, hardwareAcceleration: 'prefer-hardware' })).supported;
    } catch { return false; }
  }

  setSize(w, h) {
    if (w !== this._w || h !== this._h) this.reset();
    this._w = w;
    this._h = h;
  }

  _teardown() {
    clearTimeout(this._firstFrameTimer); this._firstFrameTimer = null;
    if (this._decoder && this._decoder.state !== 'closed') {
      try { this._decoder.close(); } catch (e) {}
    }
    this._decoder = null;
    this._codec = null;
    this._hasOutput = false;
    this._lastInputAt = null;
    this._lastProgressAt = 0;
    this._submissions.clear();
  }

  _ensureDecoder(sps) {
    const codec = 'avc1.' +
      sps[0].toString(16).padStart(2, '0') +
      sps[1].toString(16).padStart(2, '0') +
      sps[2].toString(16).padStart(2, '0');
    if (this._decoder && this._decoder.state === 'configured' && this._codec === codec) return;
    this._teardown();
    this._codec = codec;
    const decoder = new VideoDecoder({
      output: (frame) => {
        if (decoder !== this._decoder) { frame.close(); return; }
        clearTimeout(this._firstFrameTimer); this._firstFrameTimer = null;
        const outputAt = performance.now();
        const gap = outputAt - this._lastOutputAt;
        if (this._hasOutput && gap <= 1000) this._stats.outputGapMaxMs = Math.max(this._stats.outputGapMaxMs, gap);
        this._hasOutput = true; this._lastOutputAt = outputAt;
        this._lastProgressAt = this._lastOutputAt;
        const submission = this._submissions.get(frame.timestamp);
        this._submissions.delete(frame.timestamp);
        if (!submission) this._stats.unmatchedOutput++;
        const delay = submission ? Math.max(0, outputAt - submission.at) : 0;
        this._stats.output++; this._stats.decodeMs += delay;
        this._stats.decodeMaxMs = Math.max(this._stats.decodeMaxMs, delay);
        // The output still carries the source PTS when metadata lookup misses
        // (for example due to platform rounding). Never silently discard a
        // valid decoded picture solely because diagnostic metadata is absent.
        this._onFrame(frame, submission?.presentationTime ?? frame.timestamp);
      },
      error: (e) => { if (decoder !== this._decoder) return; console.warn('h264 decode error:', e.message); this._fail(e.message); },
    });
    this._decoder = decoder;
    this._decoder.configure({
      codec,
      codedWidth: this._w || undefined,
      codedHeight: this._h || undefined,
      optimizeForLatency: true,
      hardwareAcceleration: 'prefer-hardware',
    });
  }

  reset() { this._teardown(); this._pressureAt = null; this._keyRequested = false; this._waitingKey = false; this._restartPending = false; this._failed = false; }

  takeDiagnostics() {
    const queue = this._decoder?.state === 'configured' ? this._decoder.decodeQueueSize : 0;
    const pendingOutput = this._submissions.size;
    const stats = { ...this._stats, queue, pendingOutput, waitingKey: this._waitingKey,
      decodeAvgMs: this._stats.output ? Math.round(this._stats.decodeMs / this._stats.output * 10) / 10 : 0 };
    delete stats.decodeMs;
    stats.decodeMaxMs = Math.round(stats.decodeMaxMs * 10) / 10;
    stats.inputGapMaxMs = Math.round(stats.inputGapMaxMs * 10) / 10;
    stats.outputGapMaxMs = Math.round(stats.outputGapMaxMs * 10) / 10;
    this._stats = decodeStats(); this._stats.queueMax = queue; this._stats.pendingMax = pendingOutput;
    return stats;
  }

  _fail(reason) {
    this._teardown(); this._failed = true; this._onUnsupported(reason);
  }

  _requestKeyframe() {
    if (this._keyRequested) return;
    this._keyRequested = true;
    this._stats.keyRequests++;
    this._onKeyframeNeeded();
  }

  // Decode one complete access unit (pre-parsed by the server).
  decode(au, presentationTime) {
    this._stats.received++;
    if (this._failed) { this._stats.discarded++; return; }
    try {
      if (!Number.isSafeInteger(presentationTime) || presentationTime <= 0) throw new Error('Invalid H.264 presentation timestamp');
      const now = performance.now();
      if (this._lastInputAt !== null && now - this._lastInputAt <= 1000) this._stats.inputGapMaxMs = Math.max(this._stats.inputGapMaxMs, now - this._lastInputAt);
      // Screen capture can legitimately stop sending while the picture is
      // static. On resume, give retained decoder references time to produce
      // output instead of dropping deltas while waiting for a new IDR.
      if (this._lastInputAt === null || now - this._lastInputAt > 1000) this._lastProgressAt = now;
      this._lastInputAt = now;
      const { key, sps } = scanAU(au);
      this._stats.queueMax = Math.max(this._stats.queueMax, this._decoder?.decodeQueueSize || 0);
      if (this._decoder && this._decoder.decodeQueueSize > 8) {
        if (this._pressureAt === null) this._pressureAt = now;
        if (this._decoder.decodeQueueSize >= 32 || now - this._pressureAt >= 250) {
          this._waitingKey = true; this._requestKeyframe();
        }
      } else { this._pressureAt = null; }
      if (this._decoder && this._hasOutput && now - this._lastProgressAt > 1500) {
        this._waitingKey = true; this._restartPending = true; this._requestKeyframe();
      }
      // Preserve queued pictures until the first output; repeatedly closing a
      // high-resolution decoder before that output can freeze playback forever.
      if (this._waitingKey && (!key || this._decoder && !this._hasOutput && !this._restartPending)) { this._stats.discarded++; return; }
      if (this._waitingKey) { this._teardown(); this._waitingKey = false; this._restartPending = false; this._pressureAt = null; }
      if (!this._decoder || this._decoder.state !== 'configured') {
        if (!key || !sps) { this._stats.discarded++; this._requestKeyframe(); return; }
        this._ensureDecoder(sps);
      } else if (key && sps) {
        this._ensureDecoder(sps);
      }
      if (!this._decoder || this._decoder.state !== 'configured') return;
      if (key) this._keyRequested = false;
      if (!this._hasOutput && this._firstFrameTimer === null) {
        this._firstFrameTimer = setTimeout(() => {
          this._firstFrameTimer = null;
          // A decoder may retain its first picture until another arrives.
          // A quiet sender is not evidence that the decoder has failed.
          if (this._lastInputAt === null || performance.now() - this._lastInputAt > 1000) return;
          this._waitingKey = true; this._restartPending = true; this._requestKeyframe();
        }, 3000);
      }
      // Preserve source presentation timestamps. Measure decode delay
      // separately in the browser clock domain.
      this._ts = presentationTime;
      while (this._submissions.has(this._ts)) this._ts++;
      if (this._submissions.size >= 128) this._submissions.delete(this._submissions.keys().next().value);
      this._submissions.set(this._ts, { at: performance.now(), presentationTime });
      this._stats.pendingMax = Math.max(this._stats.pendingMax, this._submissions.size);
      this._decoder.decode(new EncodedVideoChunk({
        type: key ? 'key' : 'delta',
        timestamp: this._ts,
        data: au,
      }));
      this._stats.submitted++;
    } catch (e) {
      console.warn('h264 decode failed:', e.message);
      this._fail(e.message);
    }
  }
}
