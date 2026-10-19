// The configurations below match the source formats advertised by the server.
export const audioFormats = [
  { codec: 'mp4a.40.2', sampleRate: 48000, numberOfChannels: 2, description: new Uint8Array([0x11, 0x90]) },
  { codec: 'mp4a.40.39', sampleRate: 48000, numberOfChannels: 2, description: new Uint8Array([0xf8, 0xe6, 0x50, 0]) },
  { codec: 'mp4a.40.39', sampleRate: 48000, numberOfChannels: 2, description: new Uint8Array([0xf8, 0xe6, 0x40, 0]) },
  { codec: 'mp4a.40.39', sampleRate: 44100, numberOfChannels: 2, description: new Uint8Array([0xf8, 0xe8, 0x50, 0]) },
];
export async function supportedAudioFormats() {
  if (typeof AudioDecoder === 'undefined') return [];
  const supportedFormats = [];
  for (const format of audioFormats) {
    let supported = false;
    try { supported = (await AudioDecoder.isConfigSupported(format)).supported; } catch (_) {}
    if (supported) supportedFormats.push({ codec: format.codec, sampleRate: format.sampleRate,
      channels: format.numberOfChannels, config: btoa(String.fromCharCode(...format.description)),
      frames: format.codec === 'mp4a.40.2' ? 1024 : format.description[2] === 0x40 ? 512 : 480 });
  }
  return supportedFormats;
}
export async function supportedAudioCodecs() {
  return [...new Set((await supportedAudioFormats()).map(f => f.codec))];
}
export function parseEncodedAudio(bytes) {
  if (bytes.length < 3) return null;
  const n = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint16(0);
  if (n < 2 || n >= bytes.length - 2) return null;
  try {
    const meta = JSON.parse(new TextDecoder().decode(bytes.subarray(2, 2 + n)));
    if (!Number.isSafeInteger(meta.timestamp) || meta.timestamp < 0 || !Number.isInteger(meta.sampleRate) ||
        meta.sampleRate <= 0 || !Number.isInteger(meta.channels) || meta.channels < 1 || meta.channels > 8 ||
        !Number.isInteger(meta.frames) || meta.frames < 1 || meta.frames > 4096 ||
        typeof meta.codec !== 'string' || typeof meta.config !== 'string' ||
        (meta.gain !== undefined && (!Number.isFinite(meta.gain) || meta.gain < 0 || meta.gain > 1)) ||
        (meta.epoch !== undefined && (!Number.isSafeInteger(meta.epoch) || meta.epoch < 0))) return null;
    return { ...meta, description: Uint8Array.from(atob(meta.config), c => c.charCodeAt(0)), data: bytes.subarray(2 + n) };
  } catch (_) { return null; }
}
export class CompressedAudioPlayer {
  constructor(context, play, failed) { this.context = context; this.play = play; this.failed = failed; this.decoder = null; this.key = ''; this.generation = 0; this.gains = new Map(); this.gain = 1; }
  reset() {
    this.generation++;
    if (this.decoder && this.decoder.state !== 'closed') this.decoder.close();
    this.decoder = null; this.key = '';
    this.gains.clear(); this.gain = 1;
  }
  push(bytes) {
    if (!this.context() || this.context().state !== 'running') return;
    const a = parseEncodedAudio(bytes);
    if (!a) { this.reset(); this.failed('Invalid compressed audio packet'); return; }
    try {
      const key = `${a.codec}/${a.sampleRate}/${a.channels}/${a.config}/${a.epoch ?? 0}`;
      if (!this.decoder || key !== this.key) {
        this.reset(); this.key = key;
        const generation = this.generation;
        this.decoder = new AudioDecoder({
          output: data => {
            try {
              const context = this.context();
              if (generation !== this.generation || !context || context.state !== 'running') return;
              const buffer = context.createBuffer(data.numberOfChannels, data.numberOfFrames, data.sampleRate);
              for (let channel = 0; channel < data.numberOfChannels; channel++) {
                data.copyTo(buffer.getChannelData(channel), { planeIndex: channel, format: 'f32-planar' });
              }
              const gain = this.gains.get(data.timestamp) ?? this.gain;
              for (const timestamp of this.gains.keys()) if (timestamp <= data.timestamp) this.gains.delete(timestamp);
              this.play(buffer, gain);
            } catch (e) { this.reset(); this.failed(e.message); }
            finally { data.close(); }
          },
          error: e => { if (generation === this.generation) { this.reset(); this.failed(e.message); } },
        });
        this.decoder.configure({ codec: a.codec, sampleRate: a.sampleRate, numberOfChannels: a.channels, description: a.description });
      }
      if (this.decoder.decodeQueueSize > 20) { this.reset(); this.failed('Audio decode queue overflow'); return; }
      this.gain = a.gain ?? 1;
      this.gains.set(a.timestamp, this.gain);
      if (this.gains.size > 64) this.gains.delete(this.gains.keys().next().value);
      this.decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: a.timestamp,
        duration: Math.round(a.frames * 1000000 / a.sampleRate), data: a.data }));
    } catch (e) { this.reset(); this.failed(e.message); }
  }
}
