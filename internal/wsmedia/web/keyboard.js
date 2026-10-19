// Keyboard capture. Sends the physical key (KeyboardEvent.code) to
// the server, which maps it to a USB HID usage. Suppresses auto-repeat and
// releases all keys on focus loss to avoid stuck keys on the remote host.

export class Keyboard {
  constructor(target, send) {
    this._target = target;   // element to attach focus-scoped listeners to
    this._send = send;       // (Uint8Array) => void
    this._down = new Set();  // codes currently held
    this._grabbed = false;

    this._onKeyDown = this._onKeyDown.bind(this);
    this._onKeyUp = this._onKeyUp.bind(this);
    this._onBlur = () => this.releaseAll();
    this._onFocusOut = (e) => {
      if (!this._target.contains(e.relatedTarget)) this.releaseAll();
    };
  }

  grab() {
    if (this._grabbed) return;
    this._grabbed = true;
    // Listen on window so we catch keys regardless of exact focus, but only act
    // while the KVM surface has focus (checked in handlers).
    window.addEventListener('keydown', this._onKeyDown, true);
    window.addEventListener('keyup', this._onKeyUp, true);
    window.addEventListener('blur', this._onBlur);
    this._target.addEventListener('focusout', this._onFocusOut);
  }

  release() {
    if (!this._grabbed) return;
    this._grabbed = false;
    window.removeEventListener('keydown', this._onKeyDown, true);
    window.removeEventListener('keyup', this._onKeyUp, true);
    window.removeEventListener('blur', this._onBlur);
    this._target.removeEventListener('focusout', this._onFocusOut);
    this.releaseAll();
  }

  _active() {
    return document.activeElement === this._target || this._target.contains(document.activeElement);
  }

  _msg(down, code) {
    const bytes = new TextEncoder().encode(code);
    const msg = new Uint8Array(2 + bytes.length);
    msg[0] = 0x80;
    msg[1] = down ? 1 : 0;
    msg.set(bytes, 2);
    this._send(msg);
  }

  _onKeyDown(e) {
    if (!this._active()) return;
    e.preventDefault();
    e.stopPropagation();
    const code = e.code;
    if (!code) return;
    if (this._down.has(code)) return; // suppress auto-repeat
    this._down.add(code);
    this._msg(true, code);
  }

  _onKeyUp(e) {
    // A held key still needs releasing if focus moved before keyup.
    if (!this._active() && !this._down.has(e.code)) return;
    e.preventDefault();
    e.stopPropagation();
    const code = e.code;
    if (!code) return;
    if (this._down.delete(code)) this._msg(false, code);
  }

  releaseAll() {
    for (const code of this._down) this._msg(false, code);
    this._down.clear();
  }
}
