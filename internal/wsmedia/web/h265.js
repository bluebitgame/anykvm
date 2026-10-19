// HEVC is repackaged for WebCodecs; no host decoder or transcoder is involved.
export function splitAnnexB(data) {
  const units = [];
  let start = -1;
  for (let i = 0; i + 2 < data.length; i++) {
    const size = data[i] === 0 && data[i + 1] === 0
      ? (data[i + 2] === 1 ? 3 : data[i + 2] === 0 && data[i + 3] === 1 ? 4 : 0) : 0;
    if (!size) continue;
    if (start >= 0 && i > start) units.push(data.subarray(start, i));
    start = i + size;
    i += size - 1;
  }
  if (start >= 0 && start < data.length) units.push(data.subarray(start));
  return units;
}

export function parseHEVCSPS(nal) {
  if (nal.length < 3 || (nal[0] >> 1 & 63) !== 33) throw new Error('Missing HEVC SPS');
  const rbsp = [];
  let zeros = 0;
  for (const byte of nal.subarray(2)) {
    if (zeros === 2 && byte === 3) { zeros = 0; continue; }
    rbsp.push(byte);
    zeros = byte === 0 ? zeros + 1 : 0;
  }
  let bit = 0;
  const read = (count) => {
    if (count < 0 || count > 32 || bit + count > rbsp.length * 8) throw new Error('Truncated HEVC SPS');
    let value = 0;
    for (let i = 0; i < count; i++, bit++) value = value * 2 + (rbsp[bit >> 3] >> (7 - (bit & 7)) & 1);
    return value;
  };
  const skip = (count) => { while (count > 0) { const n = Math.min(count, 32); read(n); count -= n; } };
  const ue = () => {
    let zeros = 0;
    while (!read(1)) if (++zeros > 31) throw new Error('Invalid HEVC Exp-Golomb value');
    return 2 ** zeros - 1 + read(zeros);
  };
  read(4);
  const layers = read(3), nested = read(1);
  const profile = read(8), compatibility = read(32);
  const constraints = Array.from({ length: 6 }, () => read(8));
  const level = read(8);
  const sub = Array.from({ length: layers }, () => [read(1), read(1)]);
  if (layers) skip((8 - layers) * 2);
  for (const [p, l] of sub) { if (p) skip(88); if (l) skip(8); }
  ue();
  const chroma = ue();
  if (chroma > 3) throw new Error('Invalid HEVC chroma');
  const separate = chroma === 3 ? read(1) : 0;
  let width = ue(), height = ue();
  if (read(1)) {
    const left = ue(), right = ue(), top = ue(), bottom = ue();
    width -= (left + right) * (!separate && (chroma === 1 || chroma === 2) ? 2 : 1);
    height -= (top + bottom) * (!separate && chroma === 1 ? 2 : 1);
  }
  const depthLuma = ue(), depthChroma = ue();
  if (layers > 6 || width < 1 || height < 1 || width > 8192 || height > 8192 || depthLuma > 7 || depthChroma > 7) throw new Error('Invalid HEVC parameters');
  let reversed = 0;
  for (let i = 0; i < 32; i++) reversed = (reversed * 2 + (Math.floor(compatibility / 2 ** i) & 1)) >>> 0;
  const flags = [...constraints];
  while (flags.length > 1 && flags.at(-1) === 0) flags.pop();
  const space = ['', 'A', 'B', 'C'][profile >> 6];
  const codec = `hvc1.${space}${profile & 31}.${reversed.toString(16).toUpperCase()}.${profile & 32 ? 'H' : 'L'}${level}.${flags.map(v => v.toString(16).toUpperCase()).join('.')}`;
  return { codec, width, height, chroma, depthLuma, depthChroma, layers, nested, profile, compatibility, constraints, level };
}

export function hevcDecoderConfig(sets) {
  if (sets.length !== 3 || sets.some((n, i) => !n || n.length < 2 || n.length > 65535 || (n[0] >> 1 & 63) !== 32 + i)) throw new Error('Missing HEVC parameter sets');
  const info = parseHEVCSPS(sets[1]);
  // ISO/IEC 14496-15 HEVCDecoderConfigurationRecord, four-byte NAL lengths.
  const description = new Uint8Array(23 + sets.reduce((size, n) => size + 5 + n.length, 0));
  const dv = new DataView(description.buffer);
  description[0] = 1; description[1] = info.profile;
  dv.setUint32(2, info.compatibility);
  description.set(info.constraints, 6); description[12] = info.level;
  dv.setUint16(13, 0xf000); description[15] = 0xfc;
  description[16] = 0xfc | info.chroma;
  description[17] = 0xf8 | info.depthLuma; description[18] = 0xf8 | info.depthChroma;
  description[21] = (info.layers + 1) << 3 | info.nested << 2 | 3;
  description[22] = 3;
  let offset = 23;
  sets.forEach((n, i) => {
    description[offset++] = 0x80 | 32 + i;
    dv.setUint16(offset, 1); offset += 2;
    dv.setUint16(offset, n.length); offset += 2;
    description.set(n, offset); offset += n.length;
  });
  return { codec: info.codec, codedWidth: info.width, codedHeight: info.height, description, optimizeForLatency: true, hardwareAcceleration: 'prefer-hardware' };
}

function lengthPrefixed(units) {
  const data = new Uint8Array(units.reduce((size, n) => size + 4 + n.length, 0));
  const dv = new DataView(data.buffer);
  let offset = 0;
  for (const n of units) { dv.setUint32(offset, n.length); offset += 4; data.set(n, offset); offset += n.length; }
  return data;
}

const decodeStats = () => ({ received: 0, submitted: 0, output: 0, discarded: 0, keyRequests: 0, rebuilds: 0, queueMax: 0, pressureRecoveries: 0, stalledRecoveries: 0 });

export class H265Player {
  constructor(onFrame, onUnsupported = () => {}, onKeyframeRequested = () => {}) {
    this._onFrame = onFrame;
    this._onUnsupported = onUnsupported;
    this._onKeyframeRequested = onKeyframeRequested;
    this._decoder = null;
    this._config = null;
    this._sets = [null, null, null];
    this._w = 0; this._h = 0;
    this._firstFrameTimer = null;
    this._hasOutput = false;
    this._failed = false;
    this._waitingKey = false;
    this._keyframeTimer = null;
    this._restartPending = false;
    this._lastOutputAt = 0;
    this._queueHighSince = null;
    this._stats = decodeStats();
  }

  static async supported() {
    if (typeof VideoDecoder !== 'function') return false;
    for (const codec of ['hvc1.1.6.L120.B0', 'hvc1.2.4.L120.B0']) {
      try {
        if ((await VideoDecoder.isConfigSupported({ codec, optimizeForLatency: true, hardwareAcceleration: 'prefer-hardware' })).supported) return true;
      } catch {}
    }
    return false;
  }

  reset() {
    clearTimeout(this._firstFrameTimer);
    clearTimeout(this._keyframeTimer);
    this._firstFrameTimer = null;
    this._hasOutput = false;
    this._failed = false;
    this._waitingKey = false;
    this._keyframeTimer = null;
    this._restartPending = false;
    this._lastOutputAt = 0;
    this._queueHighSince = null;
    if (this._decoder && this._decoder.state !== 'closed') this._decoder.close();
    this._decoder = null; this._config = null;
    this._sets = [null, null, null];
  }

  setSize(w, h) {
    if (w !== this._w || h !== this._h) this.reset();
    this._w = w; this._h = h;
  }

  takeDiagnostics() {
    const queue = this._decoder?.state === 'configured' ? this._decoder.decodeQueueSize : 0;
    const stats = { ...this._stats, queue, waitingKey: this._waitingKey };
    this._stats = decodeStats();
    this._stats.queueMax = queue;
    return stats;
  }

  _fail(reason) {
    console.warn('hevc decode failed:', reason);
    this.reset();
    this._failed = true;
    this._onUnsupported(reason);
  }

  _waitForKeyframe() {
    this._waitingKey = true;
    if (this._keyframeTimer !== null) return;
    const request = () => {
      this._stats.keyRequests++;
      this._onKeyframeRequested();
      this._keyframeTimer = setTimeout(request, 1000);
    };
    request();
  }

  _watchFirstFrame() {
    if (this._hasOutput || this._firstFrameTimer !== null) return;
    this._firstFrameTimer = setTimeout(() => {
      this._firstFrameTimer = null;
      this._restartPending = true;
      this._stats.stalledRecoveries++;
      this._waitForKeyframe();
    }, 3000);
  }

  decode(au) {
    this._stats.received++;
    if (this._failed) { this._stats.discarded++; return; }
    try {
      const units = splitAnnexB(au);
      if (!units.length) throw new Error('Missing HEVC Annex B NAL units');
      this._watchFirstFrame();
      let key = false;
      for (const n of units) {
        if (n.length < 2) throw new Error('Truncated HEVC NAL');
        const type = n[0] >> 1 & 63;
        if (type >= 16 && type <= 21) key = true;
        if (type >= 32 && type <= 34) this._sets[type - 32] = n;
      }
      const now = performance.now();
      const queue = this._decoder?.decodeQueueSize || 0;
      this._stats.queueMax = Math.max(this._stats.queueMax, queue);
      // WebSocket bursts can temporarily queue more than eight pictures. Give
      // the decoder time to drain before dropping dependent frames and forcing
      // another expensive keyframe. Bound sustained backlog and memory use.
      if (queue > 8) {
        this._queueHighSince ??= now;
        if (!this._waitingKey && (queue >= 32 || now - this._queueHighSince >= 250)) {
          this._stats.pressureRecoveries++;
          this._waitForKeyframe();
        }
      } else {
        this._queueHighSince = null;
      }
      if (!this._waitingKey && this._hasOutput && now - this._lastOutputAt > 1500) {
        this._restartPending = true;
        this._stats.stalledRecoveries++;
        this._waitForKeyframe();
      }
      // Let queued pictures drain before replacing a slow decoder. Destroying
      // it before its first output can repeat forever on high-resolution input.
      if (this._waitingKey && (!key || !this._hasOutput && !this._restartPending)) { this._stats.discarded++; return; }
      if (key && this._sets.every(Boolean)) {
        const config = hevcDecoderConfig(this._sets);
        const old = this._config;
        const changed = !old || config.codec !== old.codec || config.description.length !== old.description.length || config.description.some((v, i) => v !== old.description[i]);
        if (changed || this._waitingKey || !this._decoder || this._decoder.state !== 'configured') {
          if (this._decoder && this._decoder.state !== 'closed') this._decoder.close();
          const decoder = new VideoDecoder({
            output: (frame) => {
              if (decoder !== this._decoder) { frame.close(); return; }
              clearTimeout(this._firstFrameTimer);
              this._firstFrameTimer = null;
              this._hasOutput = true;
              this._lastOutputAt = performance.now();
              this._stats.output++;
              this._onFrame(frame);
            },
            error: (e) => { if (decoder === this._decoder) this._fail(e.message); },
          });
          this._decoder = decoder;
          this._stats.rebuilds++;
          this._decoder.configure(config);
          this._config = config;
          this._hasOutput = false;
          this._watchFirstFrame();
        }
        this._waitingKey = false;
        this._queueHighSince = null;
        this._restartPending = false;
        clearTimeout(this._keyframeTimer);
        this._keyframeTimer = null;
      }
      if (!this._decoder || this._decoder.state !== 'configured') { this._stats.discarded++; return; }
      this._decoder.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: Math.round(performance.now() * 1000), data: lengthPrefixed(units) }));
      this._stats.submitted++;
    } catch (e) {
      this._fail(e.message);
    }
  }
}
