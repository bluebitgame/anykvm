// MP4/HLS containers stay intact across the server. Native HLS or hls.js
// handles the container, using the browser's platform media decoders.
export class ContainerPlayer {
  constructor(video, { onChange = () => {}, onNotice = () => {}, fetcher = (...args) => fetch(...args) } = {}) {
    this.video = video;
    this.onChange = onChange;
    this.onNotice = onNotice;
    this.fetcher = fetcher;
    this.state = null;
    this.hls = null;
    this.generation = 0;
    this.appliedVersion = 0;
    this.appliedSeekVersion = 0;
    this.loading = false;
    this.failed = false;
    this.lastReport = 0;
    for (const event of ['loadedmetadata', 'canplay', 'playing', 'pause', 'waiting', 'seeked', 'ended', 'error', 'volumechange']) {
      video.addEventListener(event, () => {
        if (!this.active) return;
        if (event === 'error') { this.failed = true; this.onNotice('Media playback failed'); }
        this.applyControls();
        this.onChange();
        this.report(true);
      });
    }
  }
  get active() { return this.state !== null; }
  async update(state) {
    if (!state) { this.stop(); return; }
    if (this.state?.id === state.id) {
      this.state = state;
      this.applyControls();
      this.report();
      return;
    }
    this.stop();
    if (typeof state.id !== 'string' || !/^[a-f0-9]{32}$/.test(state.id) ||
        typeof state.url !== 'string' || !state.url.startsWith(`/airplay/media/${state.id}/`)) return;
    this.state = state;
    this.loading = true;
    const generation = ++this.generation;
    this.abort = new AbortController();
    this.video.hidden = false;
    this.onChange();
    try {
      // Signed URLs may have no suffix. The proxy identifies HLS from its
      // header without demuxing or decoding the media container.
      const response = await this.fetcher(state.url, { method: 'HEAD', cache: 'no-store', signal: this.abort.signal });
      if (generation !== this.generation) return;
      if (!response.ok) throw new Error('Media resource unavailable');
      const isHLS = /mpegurl/i.test(response.headers.get('Content-Type') || '');
      if (isHLS && this.video.canPlayType('application/vnd.apple.mpegurl') &&
          (typeof globalThis.ManagedMediaSource !== 'undefined' || /Apple/.test(globalThis.navigator?.vendor || ''))) {
        this.video.src = state.url;
      } else if (isHLS) {
        const Hls = globalThis.Hls;
        if (!Hls?.isSupported()) throw new Error('HLS playback unavailable in this browser');
        this.hls = new Hls({ enableWorker: true, backBufferLength: 30, maxBufferLength: 15 });
        this.hls.on(Hls.Events.ERROR, (_, data) => {
          if (generation !== this.generation || !data.fatal) return;
          this.failed = true;
          this.onNotice('HLS playback failed');
          this.report(true);
        });
        this.hls.loadSource(state.url);
        this.hls.attachMedia(this.video);
      } else this.video.src = state.url;
      this.loading = false;
      this.applyControls();
    } catch (error) {
      if (generation !== this.generation) return;
      this.loading = false;
      this.failed = true;
      this.onNotice(error.message);
      this.report(true);
    }
  }
  applyControls() {
    const state = this.state;
    if (!state || this.loading || this.failed || this.video.readyState < 1 || this.appliedVersion === state.version) return;
    this.appliedVersion = state.version;
    if (this.appliedSeekVersion !== state.seekVersion) {
      let position = state.position;
      if (state.startFraction > 0 && Number.isFinite(this.video.duration)) position = state.startFraction * this.video.duration;
      if (Number.isFinite(position) && (position > 0 || state.seekVersion > 1)) {
        try { this.video.currentTime = position; } catch (_) { this.appliedVersion = 0; return; }
      }
      this.appliedSeekVersion = state.seekVersion;
    }
    if (state.paused) this.video.pause();
    else this.play();
  }
  async play() {
    const generation = this.generation;
    try { await this.video.play(); }
    catch (error) {
      if (generation !== this.generation || !this.active || this.state.paused) return;
      if (error.name === 'NotAllowedError') {
        this.video.muted = true;
        this.onChange();
        try { await this.video.play(); }
        catch (_) { if (generation === this.generation) this.onNotice('Click Play to start media'); }
      } else if (error.name !== 'AbortError') this.onNotice(error.message);
    }
  }
  setMuted(muted) {
    this.video.muted = muted;
    if (!this.state?.paused) this.play();
    this.onChange();
  }
  report(force = false) {
    if (!this.active) return;
    const now = Date.now();
    if (!force && now - this.lastReport < 500) return;
    this.lastReport = now;
    const spans = ranges => Array.from({ length: Math.min(ranges.length, 32) }, (_, i) => ({ start: ranges.start(i), duration: ranges.end(i) - ranges.start(i) }));
    const v = this.video;
    const finite = n => Number.isFinite(n) && n >= 0 ? n : 0;
    const feedback = {
      id: this.state.id, version: this.appliedVersion || this.state.version,
      duration: finite(v.duration), position: finite(v.currentTime),
      state: this.failed || v.ended ? 'stopped' : this.loading || v.readyState < 3 ? 'loading' : v.paused ? 'paused' : 'playing',
      ready: !this.failed && !this.loading && v.readyState >= 1,
      buffered: spans(v.buffered), seekable: spans(v.seekable),
    };
    this.fetcher('/airplay/media-state', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(feedback), cache: 'no-store' }).catch(() => {});
  }
  stop() {
    const wasActive = this.active;
    this.state = null;
    this.generation++;
    this.abort?.abort();
    this.abort = null;
    this.hls?.destroy();
    this.hls = null;
    this.appliedVersion = 0;
    this.appliedSeekVersion = 0;
    this.loading = this.failed = false;
    if (wasActive) {
      this.video.pause();
      this.video.removeAttribute('src');
      this.video.load();
      this.video.hidden = true;
      this.onChange();
    }
  }
}
