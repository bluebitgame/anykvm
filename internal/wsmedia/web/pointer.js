// Pointer input: absolute framebuffer coordinates (0x81) or signed relative
// framebuffer deltas (0x87), followed by a shared button bitmask.
// buttons: bit0 left, bit1 middle, bit2 right, bit3 wheelUp, bit4 wheelDown.
import { inverseRotation, normalizeRotation } from './rotation.js';

export class Pointer {
  constructor(canvas, send, getSize) {
    this._canvas = canvas;
    this._send = send;
    this._getSize = getSize;   // () => {w,h,rotation} original framebuffer and display transform
    this._buttons = 0;         // persistent mouse button state
    this._x = 0;
    this._y = 0;
    this._relative = false;
    this._anchor = null;
    this._fractionX = 0;
    this._fractionY = 0;
    this._pendingMotion = null;
    this._motionTimer = null;
    this._lastMotionAt = -Infinity;

    // Raw updates are dispatched without waiting for the browser's rendering
    // cadence. Never also forward their compatibility mousemove events.
    // Keep pointer-lock deltas on mousemove: raw movementX/Y semantics vary
    // between browsers, while absolute coordinates need only the latest sample.
    this._rawMotion = window.isSecureContext && 'onpointerrawupdate' in window;
    if (this._rawMotion) canvas.addEventListener('pointerrawupdate', (e) => {
      if (e.pointerType === 'mouse' && !this._relative) this._rawMouse(e);
    });
    canvas.addEventListener('mousemove', (e) => {
      if (!this._rawMotion || this._relative) this._mouse(e);
    });
    canvas.addEventListener('mousedown', (e) => {
      e.preventDefault();
      this._flushMotion();
      if (this._relative && document.pointerLockElement !== canvas && canvas.requestPointerLock) {
        try { canvas.requestPointerLock()?.catch(() => {}); } catch (_) {}
      }
      this._mouse(e);
    });
    canvas.addEventListener('mouseleave', () => { this._flushMotion(); this._anchor = null; });
    if (typeof document !== 'undefined') document.addEventListener('pointerlockchange', () => {
      if (document.pointerLockElement !== canvas) this.releaseAll();
      else { this._anchor = null; this._fractionX = this._fractionY = 0; }
    });
    // Finish drags even when the pointer leaves the canvas.
    window.addEventListener('mouseup', (e) => {
      if (this._buttons) { e.preventDefault(); this._flushMotion(); this._mouse(e); }
    });
    window.addEventListener('blur', () => this.releaseAll());
    canvas.addEventListener('contextmenu', (e) => e.preventDefault());
    canvas.addEventListener('wheel', (e) => this._wheel(e), { passive: false });

    canvas.addEventListener('touchstart', (e) => this._touch(e, true), { passive: false });
    canvas.addEventListener('touchmove', (e) => this._touch(e, false), { passive: false });
    canvas.addEventListener('touchend', (e) => this._touchEnd(e), { passive: false });
    canvas.addEventListener('touchcancel', (e) => this._touchEnd(e), { passive: false });
  }

  get rawMotionSupported() { return Boolean(this._rawMotion); }

  setMode(mode) {
    const relative = mode === 'relative';
    if (relative === this._relative) return;
    this._flushMotion();
    if (this._buttons) this.releaseAll();
    this._anchor = null;
    this._fractionX = this._fractionY = 0;
    this._relative = relative;
    this._canvas.title = relative ? 'Click to capture mouse; Esc to release' : '';
    if (!relative && document.pointerLockElement === this._canvas) document.exitPointerLock();
  }

  releaseAll() {
    this._flushMotion();
    this._anchor = null;
    this._fractionX = this._fractionY = 0;
    this._buttons = 0;
    this._sendPos(0);
  }

  _rawMouse(e) {
    // Keep only the latest position at up to 250 Hz, independently of rAF.
    // An 8 kHz gaming mouse must not queue seconds of old USB coordinates.
    this._pendingMotion = { type: 'pointerrawupdate', clientX: e.clientX, clientY: e.clientY, buttons: e.buttons };
    if (e.buttons !== this._buttons || performance.now() - this._lastMotionAt >= 4) {
      this._flushMotion();
    } else if (this._motionTimer === null) {
      this._motionTimer = setTimeout(() => this._flushMotion(), 4);
    }
  }

  _flushMotion() {
    clearTimeout(this._motionTimer);
    this._motionTimer = null;
    const motion = this._pendingMotion;
    this._pendingMotion = null;
    if (motion) {
      this._lastMotionAt = performance.now();
      this._mouse(motion);
    }
  }

  // Map client coordinates to framebuffer pixels (canvas may be CSS-scaled).
  _toFB(clientX, clientY) {
    const r = this._canvas.getBoundingClientRect();
    const { w, h, rotation = 0 } = this._getSize();
    if (r.width <= 0 || r.height <= 0) return { x: this._x, y: this._y };
    const point = inverseRotation((clientX - r.left) / r.width, (clientY - r.top) / r.height, rotation);
    let x = Math.round(point.x * (w - 1));
    let y = Math.round(point.y * (h - 1));
    x = Math.max(0, Math.min(w - 1, x));
    y = Math.max(0, Math.min(h - 1, y));
    return { x, y };
  }

  _sendPos(buttons, dx = 0, dy = 0) {
    const msg = new Uint8Array(6);
    msg[0] = this._relative ? 0x87 : 0x81;
    msg[1] = buttons & 0xff;
    const view = new DataView(msg.buffer);
    view.setUint16(2, this._relative ? dx : this._x);
    view.setUint16(4, this._relative ? dy : this._y);
    this._send(msg);
  }

  _mouse(e) {
    let delta = { x: 0, y: 0 };
    if (this._relative) {
      if (e.type === 'mousemove') {
        const locked = document.pointerLockElement === this._canvas;
        if (locked) delta = this._delta(e.movementX || 0, e.movementY || 0);
        else if (this._anchor) delta = this._delta(e.clientX - this._anchor.x, e.clientY - this._anchor.y);
      }
      this._anchor = e.type === 'mouseup' ? null : { x: e.clientX, y: e.clientY };
    } else {
      const p = this._toFB(e.clientX, e.clientY);
      this._x = p.x; this._y = p.y;
    }
    // e.buttons: 1 left, 2 right, 4 middle -> our bit0 left, bit1 mid, bit2 right
    let b = 0;
    if (e.buttons & 1) b |= 0x01;
    if (e.buttons & 4) b |= 0x02;
    if (e.buttons & 2) b |= 0x04;
    this._buttons = b;
    this._sendDelta(b, delta);
  }

  _wheel(e) {
    e.preventDefault();
    this._flushMotion();
    if (!this._relative) {
      const p = this._toFB(e.clientX, e.clientY);
      this._x = p.x; this._y = p.y;
    }
    const dir = e.deltaY < 0 ? 0x08 : 0x10; // up : down (one step per event)
    this._sendPos(this._buttons | dir);      // press edge
    this._sendPos(this._buttons);            // release edge
  }

  // Single-touch -> left button drag.
  _touch(e, start) {
    e.preventDefault();
    this._flushMotion();
    if (e.touches.length === 0) return;
    const t = e.touches[0];
    let delta = { x: 0, y: 0 };
    if (this._relative) {
      if (!start && this._anchor) delta = this._delta(t.clientX - this._anchor.x, t.clientY - this._anchor.y);
      this._anchor = { x: t.clientX, y: t.clientY };
    } else {
      const p = this._toFB(t.clientX, t.clientY);
      this._x = p.x; this._y = p.y;
    }
    const b = 0x01; // left down
    this._buttons = b;
    this._sendDelta(b, delta);
  }

  _touchEnd(e) {
    e.preventDefault();
    this.releaseAll();
  }

  _delta(dx, dy) {
    const r = this._canvas.getBoundingClientRect();
    const { w, h, rotation = 0 } = this._getSize();
    if (r.width <= 0 || r.height <= 0) return { x: 0, y: 0 };
    let xRatio = dx / r.width, yRatio = dy / r.height;
    switch (normalizeRotation(rotation)) {
      case 90: [xRatio, yRatio] = [yRatio, -xRatio]; break;
      case 180: xRatio = -xRatio; yRatio = -yRatio; break;
      case 270: [xRatio, yRatio] = [-yRatio, xRatio]; break;
    }
    this._fractionX += xRatio * w;
    this._fractionY += yRatio * h;
    const x = Math.trunc(this._fractionX), y = Math.trunc(this._fractionY);
    this._fractionX -= x; this._fractionY -= y;
    return { x, y };
  }

  _sendDelta(buttons, { x, y }) {
    // Split large motion so the signed 16-bit wire format never wraps.
    do {
      const dx = Math.max(-32768, Math.min(32767, x));
      const dy = Math.max(-32768, Math.min(32767, y));
      this._sendPos(buttons, dx, dy);
      x -= dx; y -= dy;
    } while (x || y);
  }
}
