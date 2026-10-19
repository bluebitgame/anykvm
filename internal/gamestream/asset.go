package gamestream

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// GameStream requests PNG box art separately from the XML app list. Moonlight
// desktop recognizes 628x888 as placeholder art and overlays the app's title.
// Render only requested platforms, once each, including concurrent requests.
var machineBoxArt sync.Map  // map[string]func() []byte
var webTargetCover sync.Map // map[string]func() []byte

// MachineCoverPlatform resolves a platform key to embedded artwork. Unknown
// Linux distributions retain the generic Linux logo; other sources stay plain.
func MachineCoverPlatform(platform string) string {
	if _, ok := coverLogos()[platform]; !ok {
		if strings.HasPrefix(platform, "linux-") {
			platform = "linux"
		} else {
			platform = ""
		}
	}
	return platform
}

func machineBoxArtForPlatform(platform string) []byte {
	platform = MachineCoverPlatform(platform)
	generate, _ := machineBoxArt.LoadOrStore(platform, sync.OnceValue(func() []byte {
		return renderMachineBoxArt(platform)
	}))
	return generate.(func() []byte)()
}

// MachineCoverPNG returns the shared target cover used by GameStream. Other
// frontends use this to keep target artwork consistent with Moonlight.
func MachineCoverPNG(platform string) []byte { return machineBoxArtForPlatform(platform) }

// WebTargetCoverPNG renders the same platform logo assets as GameStream in a
// landscape composition for the browser target picker. Unlike Moonlight box
// art, there is no client-side title overlay, so the logo is centered.
func WebTargetCoverPNG(platform string) []byte {
	platform = MachineCoverPlatform(platform)
	generate, _ := webTargetCover.LoadOrStore(platform, sync.OnceValue(func() []byte {
		return renderWebTargetCover(platform)
	}))
	return generate.(func() []byte)()
}

func (s *Server) appAsset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("appid"))
	if err != nil {
		reply(w, 400, "Invalid app asset ID")
		return
	}
	for i, a := range s.apps {
		if a.ID == id {
			platform := ""
			if i < len(s.cfg.MachinePlatforms) {
				platform = s.cfg.MachinePlatforms[i]
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "private, max-age=86400")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Write(machineBoxArtForPlatform(platform))
			return
		}
	}
	reply(w, 404, "Unknown target app asset")
}
