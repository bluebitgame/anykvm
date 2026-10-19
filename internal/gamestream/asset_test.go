package gamestream

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"
)

func TestAppAssetPlatforms(t *testing.T) {
	knownPlatforms := []string{
		"darwin", "windows", "linux", "android", "ios",
		"linux-debian", "linux-arch", "linux-fedora", "linux-ubuntu",
		"linux-manjaro", "linux-linuxmint", "linux-pop", "linux-endeavouros",
		"linux-opensuse", "linux-nixos", "linux-kali", "linux-rocky", "linux-almalinux",
		"linux-raspbian", "freebsd", "openbsd", "netbsd", "linux-steamos", "linux-bazzite", "linux-cachyos",
	}
	for platform := range coverLogos() {
		if !slices.Contains(knownPlatforms, platform) {
			knownPlatforms = append(knownPlatforms, platform)
		}
	}
	slices.Sort(knownPlatforms)
	platforms := append(append([]string{}, knownPlatforms...), "linux-unknown-distribution", "unknown", "")
	s := &Server{cfg: Config{MachinePlatforms: platforms[:len(platforms)-1]}}
	for i := range platforms {
		s.apps = append(s.apps, app{Title: fmt.Sprintf("machine %d", i), ID: i + 1})
	}
	images := make(map[string][]byte)
	for i, platform := range platforms {
		t.Run(platform, func(t *testing.T) {
			res := httptest.NewRecorder()
			s.appAsset(res, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/appasset?appid=%d", i+1), nil))
			if res.Code != http.StatusOK || res.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("invalid asset response: %d %v", res.Code, res.Header())
			}
			img, err := png.Decode(bytes.NewReader(res.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds() != image.Rect(0, 0, 628, 888) {
				t.Fatalf("platform art must preserve Moonlight placeholder dimensions: %v", img.Bounds())
			}
			images[platform] = bytes.Clone(res.Body.Bytes())
		})
	}
	for i, platform := range knownPlatforms {
		for _, other := range knownPlatforms[i+1:] {
			// macOS/iOS and Asahi/Fedora Asahi intentionally share source art.
			if reflect.DeepEqual(coverLogos()[platform], coverLogos()[other]) {
				continue
			}
			if bytes.Equal(images[platform], images[other]) {
				t.Errorf("%s and %s must have distinct covers", platform, other)
			}
		}
	}
	if !bytes.Equal(images["unknown"], images[""]) {
		t.Error("unknown and omitted platforms must use the generic cover")
	}
	if !bytes.Equal(images["linux-unknown-distribution"], images["linux"]) {
		t.Error("unknown Linux distributions must use the penguin cover")
	}
}

func TestWebTargetCoverIsLandscapeAndCentered(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(WebTargetCoverPNG("darwin")))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != image.Rect(0, 0, webCoverWidth, webCoverHeight) {
		t.Fatalf("web cover bounds = %v", img.Bounds())
	}
	background := img.At(0, 0)
	minY, maxY := webCoverHeight, -1
	for y := 0; y < webCoverHeight; y++ {
		for x := 0; x < webCoverWidth; x++ {
			if img.At(x, y) != background {
				minY = min(minY, y)
				maxY = max(maxY, y)
			}
		}
	}
	if maxY < minY {
		t.Fatal("web cover has no logo pixels")
	}
	logoCenter := (minY + maxY + 1) / 2
	if delta := logoCenter - webCoverHeight/2; delta < -2 || delta > 2 {
		t.Fatalf("web logo center = %d, want %d", logoCenter, webCoverHeight/2)
	}
}

func TestAppAssetRequiresPairingAndReturnsPNG(t *testing.T) {
	s := testServer(t)
	q := url.Values{"appid": {fmt.Sprint(s.apps[0].ID)}, "AssetType": {"2"}, "AssetIdx": {"0"}}
	if r := getReply(t, http.DefaultClient, plainURL(s), "/appasset", q); r.Code != 401 {
		t.Fatalf("unpaired asset: %+v", r)
	}
	if r := getReply(t, tlsClient(t, s, nil), secureURL(s), "/appasset", q); r.Code != 401 {
		t.Fatalf("unpaired HTTPS asset: %+v", r)
	}
	c := pairClient(t, s)
	client := tlsClient(t, s, &c.cert)
	res, err := client.Get(secureURL(s) + "/appasset?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" {
		t.Fatal("invalid asset response", res.Status)
	}
	img, err := png.Decode(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	// These exact dimensions trigger Moonlight desktop's native AppTitle overlay.
	if img.Bounds().Dx() != 628 || img.Bounds().Dy() != 888 {
		t.Fatal("box art must use Moonlight placeholder dimensions")
	}
	q.Set("appid", "-1")
	if r := getReply(t, client, secureURL(s), "/appasset", q); r.Code != 404 {
		t.Fatalf("unknown asset: %+v", r)
	}
	q.Set("appid", "bad")
	if r := getReply(t, client, secureURL(s), "/appasset", q); r.Code != 400 {
		t.Fatalf("invalid asset: %+v", r)
	}
}

// Keep the native title area clear and preserve bitmap palette colors exactly.
func TestCoverIconRendering(t *testing.T) {
	for platform := range coverLogos() {
		t.Run(platform, func(t *testing.T) {
			img, err := png.Decode(bytes.NewReader(machineBoxArtForPlatform(platform)))
			if err != nil {
				t.Fatal(err)
			}
			bg := img.At(0, 0)
			palette := make(map[[4]uint32]bool)
			addColor := func(c color.Color) {
				r, g, b, a := c.RGBA()
				palette[[4]uint32{r, g, b, a}] = true
			}
			addColor(coverBackground)
			logo := coverLogos()[platform]
			text, err := coverLogoFiles.ReadFile("coverlogos/" + logo.File)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range parseCoverLogo(string(text), logo.Colors) {
				for _, cell := range row {
					addColor(coverColor(cell.fg))
					if cell.bg >= 0 {
						addColor(coverColor(cell.bg))
					}
				}
			}
			colors := make(map[[4]uint32]bool)
			inkPixels := 0
			for y := 0; y < img.Bounds().Dy(); y++ {
				for x := 0; x < img.Bounds().Dx(); x++ {
					c := img.At(x, y)
					r, g, b, a := c.RGBA()
					colors[[4]uint32{r, g, b, a}] = true
					if !palette[[4]uint32{r, g, b, a}] {
						t.Fatalf("bitmap antialiasing or recoloring at (%d,%d)", x, y)
					}
					if a != 65535 {
						t.Fatal("cover must remain opaque")
					}
					if c != bg {
						inkPixels++
					}
					if y >= 366 && c != bg {
						t.Fatalf("icon overlaps title area at (%d,%d)", x, y)
					}
				}
			}
			if inkPixels == 0 {
				t.Fatal("embedded logo produced an empty cover")
			}
			if (platform == "linux-nixos" || platform == "linux-manjaro") && len(colors) > 3 {
				t.Fatalf("Unicode blocks acquired font antialiasing: %d colors", len(colors))
			}
		})
	}
}

func TestCoverLogoStyles(t *testing.T) {
	rows := parseCoverLogo("\n  $1A$$$2 B$3C$4D$5E$6F  \r\n", []string{
		"31", "47", "0", "8;44", "28;49;1;32", "22;39",
	})
	want := [][]coverCell{{
		{'A', 9, -1, false}, {'$', 9, -1, false},
		{' ', 9, 7, false}, {'B', 9, 7, false},
		{'C', 7, -1, false}, {'D', 7, 4, true},
		{'E', 10, -1, false}, {'F', 7, -1, false},
	}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("color slots, dollar escaping, background or reset lost:\ngot  %#v\nwant %#v", rows, want)
	}
	if rows := parseCoverLogo("$1 \n ", []string{"8"}); len(rows) != 0 {
		t.Fatal("invisible padding should not create a logo")
	}
	// A background-colored space must survive cropping even without foreground.
	if rows := parseCoverLogo("$1 ", []string{"47"}); len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].bg != 7 {
		t.Fatal("background-only logo lost")
	}
}

func TestCoverExtendedColors(t *testing.T) {
	rows := parseCoverLogo("$1A$2B$3C", []string{"38;5;25", "38;2;100;165;225;48;5;232", "38;5;2"})
	wantFG := []color.RGBA{{0, 95, 175, 255}, {100, 165, 225, 255}, {78, 154, 6, 255}}
	for i, cell := range rows[0] {
		if got := coverColor(cell.fg); got != wantFG[i] {
			t.Errorf("character %d: got %v, want %v", i, got, wantFG[i])
		}
	}
	if got := coverColor(rows[0][1].bg); got != (color.RGBA{8, 8, 8, 255}) {
		t.Errorf("indexed background: %v", got)
	}
}

func TestEmbeddedCoverArtwork(t *testing.T) {
	for platform, logo := range coverLogos() {
		if got := MachineCoverPlatform(platform); got != platform {
			t.Errorf("platform %q resolves to %q", platform, got)
		}
		text, err := coverLogoFiles.ReadFile("coverlogos/" + logo.File)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(platform, "linux-") && len(logo.Colors) == 0 {
			t.Errorf("missing palette for %s", platform)
		}
		rows := parseCoverLogo(string(text), logo.Colors)
		cw, ch := coverCellSize(rows)
		bitmap, scale := coverFontForCell(cw, ch)
		if len(rows[0])*cw > logoWidth || len(rows)*ch > logoHeight {
			t.Fatalf("%s: artwork exceeds the drawing area", platform)
		}
		for _, row := range rows {
			for _, cell := range row {
				if cell.hidden || unicode.IsSpace(cell.char) || coverBlockMask(cell.char) != 0 || (cell.char >= '⠀' && cell.char <= '⣿') {
					continue
				}
				if bitmap == nil {
					t.Fatalf("%s: no font fits %dx%d", platform, cw, ch)
				}
				if _, ok := bitmap.glyphs[cell.char]; !ok {
					t.Fatalf("%s: unsupported glyph %q", platform, cell.char)
				}
				if bitmap.width*scale > cw || bitmap.height*scale > ch {
					t.Fatalf("%s: font clips the character cell", platform)
				}
			}
		}
	}
}

func TestCoverBrailleDots(t *testing.T) {
	positions := [...]image.Point{{1, 1}, {1, 5}, {1, 9}, {5, 1}, {5, 5}, {5, 9}, {1, 13}, {5, 13}}
	for bits := 0; bits < 256; bits++ {
		img := image.NewRGBA(image.Rect(0, 0, 8, 16))
		drawCoverBraille(img, img.Bounds(), rune(0x2800+bits), color.White)
		for bit, p := range positions {
			if got := img.RGBAAt(p.X, p.Y).A == 255; got != (bits&(1<<bit) != 0) {
				t.Fatalf("Braille %02x: dot %d placed incorrectly", bits, bit)
			}
		}
	}
}

func TestCoverBlockQuadrants(t *testing.T) {
	// Index bits: top-left, top-right, bottom-left, bottom-right.
	for bits, c := range []rune{' ', '▘', '▝', '▀', '▖', '▌', '▞', '▛', '▗', '▚', '▐', '▜', '▄', '▙', '▟', '█'} {
		img := image.NewRGBA(image.Rect(0, 0, 8, 16))
		drawCoverBlock(img, image.Rect(0, 0, 8, 16), coverBlockMask(c), color.White)
		for y := 0; y < 16; y++ {
			for x := 0; x < 8; x++ {
				_, _, _, alpha := img.At(x, y).RGBA()
				wantInk := bits&(1<<((y/8)*2+x/4)) != 0
				if (alpha == 65535) != wantInk {
					t.Fatalf("%c has wrong quadrant or gap at (%d,%d)", c, x, y)
				}
			}
		}
	}
}

func TestCoverCacheConcurrent(t *testing.T) {
	machineBoxArt.Delete("freebsd")
	var wg sync.WaitGroup
	results := make([][]byte, 16)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = machineBoxArtForPlatform("freebsd")
		}()
	}
	wg.Wait()
	for _, result := range results {
		if &result[0] != &results[0][0] {
			t.Fatal("concurrent requests must reuse the same encoded PNG")
		}
	}
}
