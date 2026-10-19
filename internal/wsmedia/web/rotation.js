// Rotation changes presentation only; encoded dimensions and pointer protocol
// coordinates stay in the original framebuffer's coordinate system.
export function normalizeRotation(value) {
  return value === 90 || value === 180 || value === 270 ? value : 0;
}

export function inverseRotation(x, y, rotation) {
  switch (normalizeRotation(rotation)) {
    case 90: return { x: y, y: 1 - x };
    case 180: return { x: 1 - x, y: 1 - y };
    case 270: return { x: 1 - y, y: x };
    default: return { x, y };
  }
}

export function fitRotatedFrame(width, height, rotation, availableWidth, availableHeight) {
  const sideways = normalizeRotation(rotation) % 180 !== 0;
  const displayWidth = sideways ? height : width;
  const displayHeight = sideways ? width : height;
  const scale = Math.min(1, availableWidth / displayWidth, availableHeight / displayHeight);
  return { width: width * scale, height: height * scale, displayWidth: displayWidth * scale, displayHeight: displayHeight * scale };
}
