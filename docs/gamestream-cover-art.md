# GameStream cover art

Moonlight target covers are 628x888 PNGs. Artwork occupies a 564x332 area at
y=16; the area below y=348 stays clear for Moonlight's title/status overlay.
The Web target picker reuses the same assets in a centered landscape layout.
Each platform image is rendered and cached on demand.

Platform selection follows the captured source. Unknown Linux distributions use
the generic Linux logo; unknown source platforms use a plain cover. Detection
uses distribution IDs and explicit variants, not the client's OS or a desktop
environment guess. See `internal/gamestream/cover_platform.go`.

## Asset format

`internal/gamestream/coverlogos/logos.json` maps platform keys to UTF-8 text
files and ordered SGR color slots. `$1` through `$9` select a slot; `$$` emits
`$`. Standard ANSI, 256-color and RGB colors, background and conceal/reveal are
supported. Internal spaces and line positions are preserved; unused outer
padding is cropped. The renderer uses a fixed Tango ANSI palette.

Ordinary glyphs use the bundled Tamzen Bold font (5x9, 6x12, 8x16 or 10x20),
with integer scaling and no antialiasing. Unicode full/half/quadrant blocks and
Braille dots are drawn geometrically. The terminal grid has a 1:2 aspect ratio;
dense ASCII artwork may use a 5x10 cell when the smallest font would not fit.

## Adding artwork

1. Pin the upstream text asset and checksum in `tools/sources.lock.json`, then
   restore it with `python3 tools/prepare_sources.py`. Preserve shapes, color
   slots and internal spacing.
2. Add its platform key, filename and SGR slots to `coverlogos/logos.json`.
3. Extend `cover_platform.go` and its tests if new source detection is needed.
   Use the actual distribution ID; `ID_LIKE` alone is not an exact match.
4. Run `go test ./internal/gamestream` after [preparing build inputs](build.md).
   Keep the Moonlight placeholder dimensions and title area intact.

## Build inputs

Artwork and BDF fonts are downloaded and checksum-verified before compilation,
then embedded with palette metadata. Downloaded assets are
ignored by Git; palette metadata and pinned restoration rules remain versioned.
Runtime rendering needs no network or installed fonts.

## References

- [Fastfetch](https://github.com/fastfetch-cli/fastfetch)
- [Bazzite](https://github.com/ublue-os/bazzite)
- [Tamzen](https://github.com/sunaku/tamzen-font)
