// The toolbar button preserves controls; the shortcut gives video the full area.
export function bindFullscreen(button, notice, onChange) {
  const root = document.documentElement;
  const request = root.requestFullscreen || root.webkitRequestFullscreen;
  const exit = document.exitFullscreen || document.webkitExitFullscreen;
  const enabled = root.requestFullscreen ? document.fullscreenEnabled : document.webkitFullscreenEnabled;
  const supported = typeof request === 'function' && typeof exit === 'function' && enabled !== false;
  const current = () => document.fullscreenElement || document.webkitFullscreenElement;
  let pending = false;
  let mode = 'toolbar';
  let lockGeneration = 0;
  const browserKeyboard = globalThis.navigator?.keyboard;

  async function lockEscape() {
    const generation = ++lockGeneration;
    const unavailable = () => {
      notice.textContent = 'Esc may exit fullscreen: keyboard lock unavailable. Use Ctrl+Alt+Shift+X to exit.';
      notice.hidden = false;
    };
    if (typeof browserKeyboard?.lock !== 'function') { unavailable(); return; }
    try {
      // Keep a normal Esc press available to the remote keyboard handler.
      await browserKeyboard.lock(['Escape']);
      if (current() !== root) browserKeyboard.unlock();
    } catch (error) {
      if (generation === lockGeneration && current() === root) {
        unavailable();
        console.warn('Escape keyboard lock unavailable:', error);
      }
    }
  }

  function update() {
    const active = current() === root;
    root.setAttribute('data-fullscreen-mode', active ? mode : '');
    button.textContent = active && mode === 'toolbar' ? '🔳' : '🔲';
    button.title = supported ? (active && mode === 'toolbar' ? 'Exit fullscreen' : 'Enter fullscreen with toolbar') : 'Fullscreen unavailable in this browser';
    button.setAttribute('aria-label', button.title);
    button.setAttribute('aria-pressed', String(active && mode === 'toolbar'));
    button.disabled = !supported || pending;
  }

  function failed(error) {
    notice.textContent = 'Unable to switch fullscreen. Please try again.';
    notice.hidden = false;
    console.warn('Fullscreen unavailable:', error);
    pending = false;
    update();
  }

  const changed = () => {
    pending = false;
    notice.hidden = true;
    if (current() !== root) mode = 'toolbar';
    update();
    onChange(current() === root, !!current());
    if (current() === root) lockEscape();
    else {
      lockGeneration++;
      browserKeyboard?.unlock?.();
    }
  };
  document.addEventListener('fullscreenchange', changed);
  document.addEventListener('webkitfullscreenchange', changed);
  document.addEventListener('fullscreenerror', failed);
  document.addEventListener('webkitfullscreenerror', failed);

  async function toggle(desiredMode) {
    if (!supported || pending) return;
    if (current() === root && mode !== desiredMode) {
      mode = desiredMode;
      update();
      onChange(true, true);
      return;
    }
    pending = true;
    notice.hidden = true;
    update();
    try {
      // Invoke during the click gesture; older WebKit methods return no Promise.
      if (current()) await exit.call(document);
      else {
        mode = desiredMode;
        await request.call(root, { navigationUI: 'hide' });
      }
    } catch (error) {
      failed(error);
    } finally {
      pending = false;
      update();
    }
  }
  button.addEventListener('click', () => toggle('toolbar'));
  // Register before the remote keyboard capture so this shortcut stays local.
  window.addEventListener('keydown', event => {
    if (!supported || event.code !== 'KeyX' || !event.ctrlKey || !event.altKey || !event.shiftKey || event.metaKey) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    if (!event.repeat) toggle('video');
  }, true);
  update();
}
