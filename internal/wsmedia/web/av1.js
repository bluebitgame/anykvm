// AV1 Main, 8-bit WebCodecs player. The server supplies recovery status and the
// sequence header's codec string; the encoded data remains low-overhead OBUs.
export class AV1Player {
  constructor(onFrame, onUnsupported = () => {}) {
    this._onFrame = onFrame;
    this._onUnsupported = onUnsupported;
    this._decoder = null;
    this._codec = null;
    this._w = 0;
    this._h = 0;
  }

  static async supported() {
    if (typeof VideoDecoder !== 'function') return false;
    try {
      const result = await VideoDecoder.isConfigSupported({
        codec: 'av01.0.08M.08',
        optimizeForLatency: true,
        hardwareAcceleration: 'prefer-hardware',
      });
      return result.supported;
    } catch { return false; }
  }

  reset() {
    if (this._decoder && this._decoder.state !== 'closed') this._decoder.close();
    this._decoder = null;
    this._codec = null;
  }

  setSize(w, h) {
    if (w !== this._w || h !== this._h) this.reset();
    this._w = w;
    this._h = h;
  }

  decode(payload) {
    if (payload.length < 3 || payload[0] > 1) return;
    const key = payload[0] === 1;
    const size = payload[1];
    if (payload.length <= 2 + size) return;
    const codec = new TextDecoder().decode(payload.subarray(2, 2 + size));
    if (key && !/^av01\.0\.\d{2}[MH]\.08$/.test(codec)) return;
    if (this._decoder && this._decoder.decodeQueueSize > 8) this.reset();
    if (key && codec !== this._codec) this.reset();
    try {
      if (!this._decoder) {
        if (!key) return;
        this._decoder = new VideoDecoder({
          output: this._onFrame,
          error: () => { this.reset(); this._onUnsupported(); },
        });
        this._decoder.configure({
          codec, codedWidth: this._w || undefined, codedHeight: this._h || undefined,
          optimizeForLatency: true, hardwareAcceleration: 'prefer-hardware',
        });
        this._codec = codec;
      }
      this._decoder.decode(new EncodedVideoChunk({
        type: key ? 'key' : 'delta', timestamp: Math.round(performance.now() * 1000),
        data: payload.subarray(2 + size),
      }));
    } catch { this.reset(); this._onUnsupported(); }
  }
}
