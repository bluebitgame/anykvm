package app

import (
	"os"

	"anykvm/internal/airplay"
	"anykvm/internal/capture"
	"anykvm/internal/coredevice"
	"anykvm/internal/gamestream"
	"anykvm/internal/rdp"
	"anykvm/internal/rtsp"
	"anykvm/internal/scrcpy"
	"anykvm/internal/vnc"
)

func gamestreamConfig(cfg Config, pair bool) (gamestream.Config, error) {
	httpAddr, httpsAddr, err := gamestream.ListenerAddresses(cfg.GameStream.Addr)
	if err != nil {
		return gamestream.Config{}, err
	}
	state, err := gamestream.ResolveStatePath(cfg.GameStream.State)
	if err != nil {
		return gamestream.Config{}, err
	}
	if err := cfg.GameStream.validateProfile(); err != nil {
		return gamestream.Config{}, err
	}
	clientOS, err := gamestream.ParseClientOSOverrides(cfg.GameStream.ClientOS)
	if err != nil {
		return gamestream.Config{}, err
	}
	profile := cfg.GameStream.Profile
	if profile == "" {
		profile = "constrained_baseline"
	}
	g := gamestream.Config{H264Profile: profile, DisableLowLatencyRateControl: !cfg.GameStream.LowDelay, EnableAV1: cfg.GameStream.Codecs.AV1, EnableHEVC: cfg.GameStream.Codecs.HEVC, HTTPAddr: httpAddr, HTTPSAddr: httpsAddr, StateFile: state, Name: "AnyKVM", VideoPacingMbps: cfg.GameStream.PacingMbps}
	g.ClientOS = clientOS
	platforms := targetCoverPlatforms(cfg)
	for i, m := range cfg.Targets {
		g.Machines = append(g.Machines, m.Name)
		audioEnabled := m.Audio.Source != "" && (m.Audio.Source != "auto" || capture.IsScreenDevice(m.Video.Source) || vnc.IsVNCDevice(m.Video.Source) || airplay.IsDevice(m.Video.Source) || capture.IsIOSDevice(m.Video.Source) || coredevice.IsDevice(m.Video.Source) || rtsp.IsDevice(m.Video.Source) || rdp.IsSource(m.Video.Source))
		g.MachineAudio = append(g.MachineAudio, audioEnabled)
		g.MachinePlatforms = append(g.MachinePlatforms, platforms[i])
	}
	if pair {
		g.RequestPIN = gamestream.TerminalPIN(os.Stdin)
	}
	return g, nil
}

// targetCoverPlatforms is shared by GameStream and the browser target picker
// so a target has the same platform artwork in both clients.
func targetCoverPlatforms(cfg Config) []string {
	platforms := make([]string, 0, len(cfg.Targets))
	screenPlatform := gamestream.ScreenPlatform()
	for _, target := range cfg.Targets {
		platform := ""
		switch {
		case capture.IsScreenDevice(target.Video.Source):
			platform = screenPlatform
		case scrcpy.IsAndroidDevice(target.Video.Source):
			platform = "android"
		case capture.IsIOSDevice(target.Video.Source) || coredevice.IsDevice(target.Video.Source) || airplay.IsDevice(target.Video.Source):
			platform = "ios"
		}
		platforms = append(platforms, platform)
	}
	return platforms
}
