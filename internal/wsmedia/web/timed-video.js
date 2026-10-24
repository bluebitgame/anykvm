// Timestamped H.264/HEVC payload: little-endian playback microseconds, then Annex-B.
// The origin is arbitrary (Windows uses QPC). Both codecs preserve the source
// clock; only intervals matter. Reject values that cannot be represented exactly.
export function parseTimedVideo(payload) {
  if (payload.byteLength <= 8) return null;
  const view = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
  const timestamp = view.getUint32(0, true) + view.getUint32(4, true) * 0x100000000;
  if (!Number.isSafeInteger(timestamp) || timestamp <= 0) return null;
  return { timestamp, data: payload.subarray(8) };
}
