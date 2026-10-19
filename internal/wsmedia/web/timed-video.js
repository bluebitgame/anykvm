// Timestamped H.264 payload: little-endian Unix microseconds, then Annex-B.
// Number can represent current Unix microseconds exactly; reject unsafe values.
export function parseTimedH264(payload) {
  if (payload.byteLength <= 8) return null;
  const view = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
  const timestamp = view.getUint32(0, true) + view.getUint32(4, true) * 0x100000000;
  if (!Number.isSafeInteger(timestamp) || timestamp <= 0) return null;
  return { timestamp, data: payload.subarray(8) };
}
