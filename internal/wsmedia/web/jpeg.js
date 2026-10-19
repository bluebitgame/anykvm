// Bound decoding work and retained pictures when the browser falls behind.
export class JPEGPlayer {
  constructor(draw, decode = (bytes) => createImageBitmap(new Blob([bytes], { type: 'image/jpeg' }))) {
    this.draw = draw;
    this.decode = decode;
    this.pending = null;
    this.busy = false;
    this.generation = 0;
  }

  push(bytes) {
    this.pending = bytes;
    if (!this.busy) this.run();
  }

  reset() {
    this.generation++;
    this.pending = null;
  }

  async run() {
    this.busy = true;
    try {
      while (this.pending !== null) {
        const bytes = this.pending, generation = this.generation;
        this.pending = null;
        let bitmap;
        try {
          bitmap = await this.decode(bytes);
          if (generation === this.generation) this.draw(bitmap);
        } catch (_) {
          // A damaged JPEG must not stop subsequent pictures.
        } finally {
          if (bitmap) bitmap.close();
        }
      }
    } finally {
      this.busy = false;
    }
  }
}
