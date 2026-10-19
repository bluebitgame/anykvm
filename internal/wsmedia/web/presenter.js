// Source PTS estimates cadence; only the local display clock schedules pictures. Keep
// short decoded bursts, but never replay an unbounded queue or change H.264 references.
export class TimestampFramePresenter {
  constructor(draw, {
    schedule = callback => requestAnimationFrame(callback),
    cancel = handle => cancelAnimationFrame(handle),
    now = () => performance.now(),
    queueFrames = 1,
  } = {}) {
    this.draw = draw; this.schedule = schedule; this.cancel = cancel; this.now = now;
    this.queueCapacity = Number.isInteger(queueFrames) && queueFrames >= 1 && queueFrames <= 8 ? queueFrames : 1;
    this.bufferMs = this.queueCapacity > 1 ? 1000 / 60 : 0;
    this.nextDrawAt = null; this.sourceIntervals = []; this.frameIntervalMs = 1000 / 60;
    this.queue = []; this.handle = null; this.running = false; this.timed = false;
    this.lastSource = this.lastOutputAt = this.lastDrawnSource = null;
    this.backlogSince = this.lastRefreshAt = null;
    this.superseded = this.rebases = this.waitMaxMs = this.queueMax = 0;
    this.refreshCallbacks = this.refreshGapMaxMs = this.sourceGapMaxMs = 0;
    this.outputSinceRefresh = this.outputBurstMax = this.waitedRefreshes = 0;
    this.overflowDrops = this.catchUpDrops = 0;
    this.lastDrawAt = null; this.drawGapMaxMs = 0;
    this.intervalSamples = this.intervalErrorSum = this.intervalErrorAbsSum = this.intervalErrorAbsMax = 0;
  }

  clearPending() {
    for (const item of this.queue) item.frame.close();
    this.queue = []; this.backlogSince = null;
  }

  setQueueCapacity(frames) {
    const capacity = Number.isInteger(frames) && frames >= 1 && frames <= 8 ? frames : 1;
    if (this.queueCapacity === capacity) return;
    this.reset();
    this.queueCapacity = capacity;
    this.bufferMs = capacity > 1 ? 1000 / 60 : 0;
  }

  get maxWaitMs() { return Math.max(50, this.queueCapacity * 1000 / 60); }

  reset() {
    this.running = false;
    if (this.handle !== null) this.cancel(this.handle);
    this.handle = null;
    this.clearPending(); this.timed = false;
    this.nextDrawAt = null; this.sourceIntervals = []; this.frameIntervalMs = 1000 / 60;
    this.lastSource = this.lastOutputAt = this.lastDrawnSource = this.lastRefreshAt = null;
    this.outputSinceRefresh = 0;
    this.lastDrawAt = null;
  }

  push(frame, source) {
    if (!Number.isSafeInteger(source) || source <= 0) { frame.close(); return; }
    const at = this.now();
    const sourceGap = this.lastSource === null ? 0 : (source - this.lastSource) / 1000;
    // Quiet resumes or a source clock reset must not display old pending pictures.
    if (sourceGap > 1000 || sourceGap < -100 || this.lastOutputAt !== null && at - this.lastOutputAt > 1000) {
      this.superseded += this.queue.length;
      this.clearPending(); this.lastDrawnSource = null; this.rebases++;
      this.lastDrawAt = null;
      this.nextDrawAt = null; this.sourceIntervals = []; this.frameIntervalMs = 1000 / 60;
    }
    if (this.lastDrawnSource !== null && source <= this.lastDrawnSource) {
      frame.close(); this.superseded++; return;
    }
    if (sourceGap > 0 && sourceGap <= 1000) this.sourceGapMaxMs = Math.max(this.sourceGapMaxMs, sourceGap);
    // Median intervals reject source-clock adjustments and sparse screen updates.
    // Absolute PTS must not hold an already decoded picture behind a future clock.
    if (Math.abs(sourceGap) >= 4 && Math.abs(sourceGap) <= 100) {
      this.sourceIntervals.push(Math.abs(sourceGap));
      if (this.sourceIntervals.length > 15) this.sourceIntervals.shift();
      const sorted = [...this.sourceIntervals].sort((a, b) => a - b);
      this.frameIntervalMs = sorted[Math.floor((sorted.length - 1) / 2)];
    }
    if (this.nextDrawAt === null) this.nextDrawAt = at + this.bufferMs;
    this.lastSource = source; this.lastOutputAt = at; this.timed = true;
    this.outputBurstMax = Math.max(this.outputBurstMax, ++this.outputSinceRefresh);
    this.queue.push({ frame, source, arrived: at });
    this.queue.sort((a, b) => a.source - b.source);
    const limit = this.queueCapacity;
    while (this.queue.length > limit) { this.queue.shift().frame.close(); this.superseded++; this.overflowDrops++; }
    // New output can arrive while a slow cadence deadline is already pending.
    // Start draining early enough to fit the burst at up to 60 display slots/s.
    const drainSlot = Math.min(this.frameIntervalMs, 1000 / 60);
    for (let i = 0; i < this.queue.length; i++) {
      this.nextDrawAt = Math.min(this.nextDrawAt,
        this.queue[i].arrived + this.maxWaitMs - i * drainSlot);
    }
    this.queueMax = Math.max(this.queueMax, this.queue.length);
    this.running = true;
    this.scheduleRefresh();
  }

  scheduleRefresh() {
    if (this.handle !== null || !this.running) return;
    this.handle = this.schedule(() => {
      this.handle = null;
      const at = this.now();
      this.refreshCallbacks++;
      this.outputSinceRefresh = 0;
      if (this.lastRefreshAt !== null) this.refreshGapMaxMs = Math.max(this.refreshGapMaxMs, at - this.lastRefreshAt);
      const refreshMs = this.lastRefreshAt === null ? 1000 / 60 : at - this.lastRefreshAt;
      this.lastRefreshAt = at;
      while (this.lastDrawnSource !== null && this.queue[0]?.source <= this.lastDrawnSource) {
        this.queue.shift().frame.close(); this.superseded++;
      }
      // Select the nearest refresh to the local cadence deadline. A small
      // callback jitter must not turn a 60fps stream into alternating 30fps.
      const tolerance = Math.min(this.frameIntervalMs / 4, Math.max(0.01, refreshMs / 4));
      const due = this.bufferMs === 0 || this.nextDrawAt <= at + tolerance;
      // Source PTS may already be old at decoder output. Only time actually
      // spent in this display queue warrants dropping a fresh decoded burst.
      const catchUp = this.queue.length && this.queue[0].arrived < at - this.maxWaitMs - 0.01 ||
        this.backlogSince !== null && at - this.backlogSince >= 500;
      let picture = due ? this.queue.shift() : null;
      if (!picture && this.queue.length) this.waitedRefreshes++;
      while (picture && catchUp && this.queue.length) {
        picture.frame.close(); this.superseded++; this.catchUpDrops++;
        picture = this.queue.shift();
      }
      try {
        if (picture) {
          this.waitMaxMs = Math.max(this.waitMaxMs, at - picture.arrived);
          // Do not carry clock debt through a stall, or move the clock on
          // every small refresh delay. Both cause uneven output on recovery.
          if (at - this.nextDrawAt > this.frameIntervalMs) {
            this.nextDrawAt = at; this.rebases++;
          }
          // Every retained picture has a residence deadline. A slow source
          // cadence must not schedule a decoded burst past those deadlines.
          // Leave one local cadence slot per remaining picture, accelerating
          // only as needed to drain the bounded queue before it becomes stale.
          let drainInterval = this.frameIntervalMs;
          for (let i = 0; i < this.queue.length; i++) {
            drainInterval = Math.min(drainInterval,
              Math.max(0, (this.queue[i].arrived + this.maxWaitMs - at) / (i + 1)));
          }
          this.nextDrawAt = Math.min(this.nextDrawAt + this.frameIntervalMs, at + drainInterval);
          // Measure successful canvas submissions, not decoder arrivals or
          // physical monitor scan-out (which canvas does not expose).
          const drawAt = this.now();
          this.draw(picture.frame);
          if (this.lastDrawAt !== null) {
            const drawInterval = drawAt - this.lastDrawAt;
            this.drawGapMaxMs = Math.max(this.drawGapMaxMs, drawInterval);
            const ptsInterval = (picture.source - this.lastDrawnSource) / 1000;
            // Include ordinary stalls and frame skips. Exclude long idle/
            // background intervals and source clock discontinuities.
            if (this.lastDrawnSource !== null && ptsInterval > 0 && ptsInterval <= 1000 && drawInterval >= 0 && drawInterval <= 1000) {
              const error = drawInterval - ptsInterval;
              this.intervalSamples++;
              this.intervalErrorSum += error;
              this.intervalErrorAbsSum += Math.abs(error);
              this.intervalErrorAbsMax = Math.max(this.intervalErrorAbsMax, Math.abs(error));
            }
          }
          this.lastDrawnSource = picture.source;
          this.lastDrawAt = drawAt;
        }
      } finally {
        if (picture) picture.frame.close();
        if (!this.queue.length || this.nextDrawAt > at + tolerance) this.backlogSince = null;
        else if (this.backlogSince === null) this.backlogSince = at;
        // Continue while this H.264 session is active, including quiet screens.
        // This measures browser refresh opportunities independently of source FPS.
        this.scheduleRefresh();
      }
    });
  }

  takeDiagnostics() {
    const round = value => Math.round(value * 10) / 10;
    const stats = {
      presentationTimed: this.timed,
      presentationMode: this.bufferMs > 0 ? 'source-cadence-budget-vsync' : 'latest-frame-vsync',
      presentationTargetBufferMs: round(this.bufferMs),
      presentationQueueCapacity: this.queueCapacity,
      presentationFrameIntervalMs: round(this.frameIntervalMs),
      presentationDrawGapMaxMs: round(this.drawGapMaxMs),
      presentationIntervalSamples: this.intervalSamples,
      presentationIntervalErrorMeanMs: this.intervalSamples ? round(this.intervalErrorSum / this.intervalSamples) : null,
      presentationIntervalErrorAbsMeanMs: this.intervalSamples ? round(this.intervalErrorAbsSum / this.intervalSamples) : null,
      presentationIntervalErrorAbsMaxMs: this.intervalSamples ? round(this.intervalErrorAbsMax) : null,
      presentationQueue: this.queue.length, presentationQueueMax: this.queueMax,
      presentationBufferedMs: this.bufferMs > 0 && this.queue.length ? round(Math.max(0, this.nextDrawAt - this.now()) + (this.queue.length - 1) * this.frameIntervalMs) : 0,
      presentationWaitMaxMs: round(this.waitMaxMs),
      presentationSuperseded: this.superseded, presentationRebases: this.rebases,
      presentationOverflowDrops: this.overflowDrops, presentationCatchUpDrops: this.catchUpDrops,
      decodedBurstMax: this.outputBurstMax, presentationWaitedRefreshes: this.waitedRefreshes,
      refreshCallbacks: this.refreshCallbacks, refreshGapMaxMs: round(this.refreshGapMaxMs),
      sourceGapMaxMs: round(this.sourceGapMaxMs),
    };
    this.superseded = this.rebases = this.waitMaxMs = 0;
    this.refreshCallbacks = this.refreshGapMaxMs = this.sourceGapMaxMs = 0;
    this.outputBurstMax = this.outputSinceRefresh;
    this.overflowDrops = this.catchUpDrops = this.waitedRefreshes = 0;
    this.drawGapMaxMs = 0;
    this.intervalSamples = this.intervalErrorSum = this.intervalErrorAbsSum = this.intervalErrorAbsMax = 0;
    this.queueMax = this.queue.length;
    return stats;
  }
}
