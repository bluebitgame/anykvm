package app

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	"anykvm/internal/airplay"
	"anykvm/internal/avmedia"
	"anykvm/internal/awdl"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/discovery"
	"anykvm/internal/gamestream"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/logging"
	"anykvm/internal/netstats"
	"anykvm/internal/pixelops"
	"anykvm/internal/rdp"
	"anykvm/internal/tlsconfig"
	"anykvm/internal/viewer"
	"anykvm/internal/vnc"
	"anykvm/internal/wsmedia"
)

const (
	defaultWidth  = 640
	defaultHeight = 480
)

func runConfiguredApplication(opts *serverOptions, configPath string, cfg Config) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	discovery.StartConfigDeviceDiscovery()
	// Keep the persisted values separate from runtime TLS defaults. A settings
	// save must not turn an omitted TLS path into a machine-specific path.
	persistedCfg := cfg

	if cfg.TLSCert == "" || cfg.TLSKey == "" {
		certFile, keyFile, generated, err := tlsconfig.EnsureDefault()
		if err != nil {
			log.Printf("AnyKVM: default TLS certificate error: %v (falling back to in-memory self-signed)", err)
		} else {
			cfg.TLSCert = certFile
			cfg.TLSKey = keyFile
			if generated {
				log.Printf("AnyKVM: generated self-signed TLS certificate: %s", certFile)
			} else {
				log.Printf("AnyKVM: using default TLS certificate: %s", certFile)
			}
		}
	}

	log.Printf("AnyKVM: JPEG backend %s", jpegcodec.Backend())
	if err := jpegcodec.PrepareD3D11Shaders(); err != nil {
		log.Fatalf("AnyKVM: prepare D3D11 JPEG shaders: %v", err)
	}
	log.Printf("AnyKVM: pixel operations %s (dirty blocks=%dx%d)", pixelops.Backend(), pixelops.BlockSize, pixelops.BlockSize)
	if !avmedia.Enabled {
		log.Printf("AnyKVM: system-native media build (FFmpeg disabled)")
	} else if err := avmedia.Available(); err == nil {
		log.Printf("AnyKVM: in-process FFmpeg libraries available (libavcodec)")
	} else {
		log.Printf("AnyKVM: %v; system-native paths remain available; put libraries in the executable lib directory or set ANYKVM_FFMPEG_LIBDIR", err)
	}

	for _, m := range cfg.Targets {
		if err := validateARDMachine(m); err != nil {
			log.Fatal(err)
		}
	}
	machines := buildMachines(cfg)
	if len(machines) == 0 {
		log.Fatal("AnyKVM: configure at least one target")
	}
	coverPlatforms := targetCoverPlatforms(cfg)
	webTargets := make([]wsmedia.Target, 0, len(machines))
	for i, machine := range machines {
		platform := ""
		if i < len(coverPlatforms) {
			platform = coverPlatforms[i]
		}
		webTargets = append(webTargets, wsmedia.Target{Name: machine.Name, Platform: platform})
	}
	// Publish AirPlay discovery before a viewer opens; the phone can mirror
	// before or after GameStream starts its waiting video.
	for _, machine := range machines {
		if receiver, ok := machine.Capture.(*airplay.Device); ok {
			if cfg.Web.HTTP != "" || cfg.Web.HTTPS != "" {
				receiver.EnableBrowserMedia()
			}
			if err := receiver.StartReceiver(machine.CapW, machine.CapH, machine.FPS); err != nil {
				log.Fatalf("AirPlay receiver for %q: %v", machine.Name, err)
			}
			defer receiver.ShutdownReceiver()
		}
	}
	b := bridge.NewBridge(machines, cfg.HotkeyMod, 0, 0, 0)
	go netstats.Run(ctx, b.RemoteControlActive)
	b.SetRDPBitrate(cfg.RDP.Bitrate)
	b.SetWebBitrate(cfg.Web.Bitrate)
	b.SetWebJPEGQuality(cfg.Web.JPEGQuality)
	b.SetVNCBitrate(cfg.VNC.Bitrate)
	b.SetRDPEncoderBufferFrames(cfg.RDP.BufferFrames)
	if cfg.DisableAWDL {
		guard := awdl.New()
		b.SetClientActivityHandler(guard.SetActive)
		defer guard.Close()
	}

	var gsSrv *gamestream.Server
	if cfg.GameStream.Addr != "" {
		gsCfg, err := gamestreamConfig(cfg, opts.gamestreamPair)
		if err != nil {
			log.Fatalf("GameStream config: %v", err)
		}
		gsCfg.SupportsHEVC = b.SupportsGameStreamHEVC
		gsCfg.SupportsAV1 = b.SupportsGameStreamAV1
		gsCfg.OnVideoRestart = b.RestartGameStreamVideo
		gsCfg.OnVideoKeyframe = b.RequestGameStreamKeyframe
		gsCfg.OnVideoRestartAllowed = b.GameStreamCanRestartForKeyframe
		gsCfg.OnVideoPlaceholderAllowed = b.GameStreamCanEncodePlaceholder
		gsCfg.OnStreamStart, gsCfg.OnStreamStop, gsCfg.HID = b.StartGameStream, b.StopGameStream, b
		gs, err := gamestream.NewServer(gsCfg)
		if err != nil {
			log.Fatalf("GameStream: %v", err)
		}
		defer gs.Close()
		gsSrv = gs
		b.SetGameStream(gs, gs)
		log.Printf("AnyKVM: experimental GameStream H.264/HEVC/AV1/Opus server on %s (HTTPS %s)", gsCfg.HTTPAddr, gsCfg.HTTPSAddr)
		log.Printf("AnyKVM: GameStream identity: %s; new pairings enabled: %t", gsCfg.StateFile, opts.gamestreamPair)
	}

	m := machines[0]
	srvW, srvH := m.FallbackW, m.FallbackH
	if srvW <= 0 {
		srvW = defaultWidth
	}
	if srvH <= 0 {
		srvH = defaultHeight
	}

	var hidDev vnc.HIDSender = b

	var vncSrv *vnc.Server
	if cfg.VNC.Addr != "" {
		vncSrv = vnc.NewServer(vnc.Config{
			Codecs:               &cfg.VNC.Codecs,
			Addr:                 cfg.VNC.Addr,
			Width:                srvW,
			Height:               srvH,
			FPS:                  m.FPS,
			Name:                 "AnyKVM",
			Password:             cfg.VNC.Password,
			TLSCertFile:          cfg.TLSCert,
			TLSKeyFile:           cfg.TLSKey,
			VNCTLS:               cfg.VNC.TLS,
			PixelTol:             cfg.VNC.PixelTol,
			JPEGQuality:          cfg.VNC.JPEGQuality,
			ForceContinuous:      cfg.VNC.ForceCont,
			CursorCapture:        b.NewCursorCapture(capture.NewCursorCapture()),
			OnClientConnecting:   b.ClaimVNC,
			OnClientConnected:    b.OnClientConnected,
			OnClientDisconnected: b.OnClientDisconnected,
			OnPixelNeedChanged:   b.SetPixelNeed,
			OnVideoH264Changed:   b.SetVideoWantsH264,
			MouseMode:            m.MouseMode,
			HID:                  hidDev,
		})
		b.SetVNC(vncSrv)
	}

	var wsSrv *wsmedia.Server
	var rdpSrv *rdp.Server
	if cfg.RDP.Addr != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			log.Fatalf("RDP TLS certificate: %v", err)
		}
		rdpSrv, err = rdp.NewServer(rdp.Config{
			DriveRedirection: cfg.RDP.Drive,
			Addr:             cfg.RDP.Addr, Username: cfg.RDP.Username, Password: cfg.RDP.Password,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
			Bitrate:   cfg.RDP.Bitrate, AllowedCodecs: &cfg.RDP.Codecs, HID: b,
			EnableHEVC:                 cfg.RDP.Codecs.HEVC,
			DisableClipboard:           !cfg.RDP.Clipboard,
			ClipboardOptions:           &cfg.RDP.ClipboardOptions,
			DisableAutoShortcutMapping: !cfg.RDP.AutoShortcutMapping,
			ProgressiveQuantization:    cfg.RDP.ProgressiveQuantization,
			ProgressiveRefinement:      cfg.RDP.ProgressiveRefinement,
			CursorCapture:              b.NewCursorCapture(capture.NewCursorCapture()),
			OnSessionStart:             b.StartRDPWithDesktop, OnDisconnected: b.OnClientDisconnected,
			OnDesktopResize: b.ResizeRDPDesktopWithOptions,
			OnKeyframe:      b.RequestRDPKeyframe,
		})
		if err != nil {
			log.Fatalf("RDP config: %v", err)
		}
		b.SetRDP(rdpSrv)
	}
	if cfg.Web.HTTP != "" || cfg.Web.HTTPS != "" {
		wsSrv = wsmedia.NewServer(wsmedia.Config{
			User:          cfg.Web.Username,
			Password:      cfg.Web.Password,
			AdminPassword: cfg.AdminPassword,
			AdminHandler: newWebConfigHandler(configPath, persistedCfg, func(previous, next Config) configSaveResult {
				result := configSaveResult{RestartReasons: restartReasons(previous, next)}
				if previous.Web.Username != next.Web.Username || previous.Web.Password != next.Web.Password || previous.AdminPassword != next.AdminPassword {
					wsSrv.UpdateAccess(next.Web.Username, next.Web.Password, next.AdminPassword)
					result.Applied = append(result.Applied, "Web authentication")
					result.SettingsAuthenticationChanged = previous.AdminPassword != next.AdminPassword
				}
				if previous.Web.Bitrate != next.Web.Bitrate {
					b.SetWebBitrate(next.Web.Bitrate)
					result.Applied = append(result.Applied, "Web video bitrate (new connections)")
				}
				if previous.Web.JPEGQuality != next.Web.JPEGQuality {
					b.SetWebJPEGQuality(next.Web.JPEGQuality)
					result.Applied = append(result.Applied, "Web JPEG quality (new connections)")
				}
				if previous.Web.BufferFrames != next.Web.BufferFrames {
					wsSrv.UpdateVideoBufferFrames(next.Web.BufferFrames)
					result.Applied = append(result.Applied, "Web video buffer (new connections)")
				}
				if previous.Web.Codecs != next.Web.Codecs {
					wsSrv.UpdateVideoCodecs(next.Web.Codecs.H264, next.Web.Codecs.HEVC, next.Web.Codecs.AV1)
					result.Applied = append(result.Applied, "Web codecs (new connections)")
				}
				if previous.VNC.Bitrate != next.VNC.Bitrate {
					b.SetVNCBitrate(next.VNC.Bitrate)
					result.Applied = append(result.Applied, "VNC video bitrate (new connections)")
				}
				if vncSrv != nil && (previous.VNC.Codecs != next.VNC.Codecs || previous.VNC.PixelTol != next.VNC.PixelTol || previous.VNC.JPEGQuality != next.VNC.JPEGQuality || previous.VNC.ForceCont != next.VNC.ForceCont) {
					vncSrv.UpdateEncoding(next.VNC.PixelTol, next.VNC.JPEGQuality, next.VNC.ForceCont)
					vncSrv.UpdateCodecs(next.VNC.Codecs)
					result.Applied = append(result.Applied, "VNC encoding (new connections)")
				}
				if rdpSrv != nil && (previous.RDP.Username != next.RDP.Username || previous.RDP.Password != next.RDP.Password || previous.RDP.Codecs != next.RDP.Codecs || previous.RDP.Bitrate != next.RDP.Bitrate || previous.RDP.BufferFrames != next.RDP.BufferFrames || previous.RDP.ProgressiveQuantization != next.RDP.ProgressiveQuantization || previous.RDP.ProgressiveRefinement != next.RDP.ProgressiveRefinement) {
					rdpSrv.UpdateRuntime(rdp.RuntimeConfig{Username: next.RDP.Username, Password: next.RDP.Password, AllowedCodecs: &next.RDP.Codecs, Bitrate: next.RDP.Bitrate, ProgressiveQuantization: next.RDP.ProgressiveQuantization, ProgressiveRefinement: next.RDP.ProgressiveRefinement})
					b.SetRDPBitrate(next.RDP.Bitrate)
					b.SetRDPEncoderBufferFrames(next.RDP.BufferFrames)
					result.Applied = append(result.Applied, "RDP settings (new connections)")
				}
				if !reflect.DeepEqual(previous.Targets, next.Targets) && gsSrv == nil {
					replacement := buildMachines(next)
					if err := b.ReplaceMachines(replacement); err != nil {
						result.RestartReasons = append(result.RestartReasons, "Target configuration")
					} else {
						machines = replacement // shutdown cleanup must close replacement HID devices.
						coverPlatforms := targetCoverPlatforms(next)
						targets := make([]wsmedia.Target, 0, len(replacement))
						for i, machine := range replacement {
							platform := ""
							if i < len(coverPlatforms) {
								platform = coverPlatforms[i]
							}
							targets = append(targets, wsmedia.Target{Name: machine.Name, Platform: platform})
						}
						wsSrv.UpdateTargets(targets)
						result.RestartReasons = withoutRestartReason(result.RestartReasons, "Target configuration")
						result.Applied = append(result.Applied, "Targets (current clients disconnected)")
					}
				}
				result.RestartRequired = len(result.RestartReasons) != 0
				return result
			}),
			Targets:                  webTargets,
			TargetCover:              gamestream.WebTargetCoverPNG,
			SettingsEnabled:          true,
			OnTargetSelected:         b.StartWeb,
			HTTPAddr:                 cfg.Web.HTTP,
			HTTPSAddr:                cfg.Web.HTTPS,
			HTTPVideoBufferFrames:    cfg.Web.BufferFrames,
			HTTPSVideoBufferFrames:   cfg.Web.BufferFrames,
			Name:                     "AnyKVM",
			TLSCertFile:              cfg.TLSCert,
			TLSKeyFile:               cfg.TLSKey,
			OnClientConnected:        b.OnClientConnected,
			OnClientDisconnected:     b.OnClientDisconnected,
			HID:                      hidDev,
			MouseMode:                b.MouseMode,
			DisableHEVC:              !cfg.Web.Codecs.HEVC,
			DisableH264:              !cfg.Web.Codecs.H264,
			EnableAV1:                cfg.Web.Codecs.AV1,
			OnVideoCodecsChanged:     b.SetVideoCodecs,
			OnVideoKeyframeRequested: b.RequestVideoKeyframe,
			BrowserMedia:             b,
		})
		b.SetWS(wsSrv, wsSrv)

		if err := wsSrv.Start(); err != nil {
			log.Printf("AnyKVM: WebSocket server start error: %v", err)
		} else {
			if cfg.Web.HTTP != "" {
				log.Printf("AnyKVM: WebSocket media server on http://%s", cfg.Web.HTTP)
			}
			if cfg.Web.HTTPS != "" {
				log.Printf("AnyKVM: WebSocket media server on https://%s", cfg.Web.HTTPS)
			}
			if cfg.AdminPassword != "" {
				log.Printf("AnyKVM: Web configuration available at /settings/")
			}
		}
	}

	// Log audio status.
	audioEnabled := false
	for _, ms := range machines {
		if ms.AudioDev != nil {
			audioEnabled = true
			break
		}
	}
	if !audioEnabled && (cfg.Web.HTTP != "" || cfg.Web.HTTPS != "") {
		log.Printf("AnyKVM: audio disabled (enable with per-target audio.source)")
	}

	if err := b.Start(); err != nil {
		log.Fatalf("start error: %v", err)
	}
	if gsSrv != nil {
		if err := gsSrv.Start(); err != nil {
			log.Fatalf("GameStream start: %v", err)
		}
	}
	if rdpSrv != nil {
		if err := rdpSrv.Start(); err != nil {
			log.Fatalf("RDP start: %v", err)
		}
	}

	defer func() {
		for _, ms := range machines {
			if ms.HID != nil {
				if c, ok := ms.HID.(interface{ Close() error }); ok {
					c.Close()
				}
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	stopViewer := make(chan struct{})
	viewerStopped := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
		case <-opts.serviceStop:
		case <-stopViewer:
		}
		close(viewerStopped)
	}()
	if cfg.Viewer && viewer.Available {
		if err := viewer.Run(b, machines[0].MouseMode, viewerStopped); errors.Is(err, viewer.ErrTakenOver) {
			logging.Info("AnyKVM: remote client took over; continuing headless")
			runApplicationMain(Config{}, func() { <-viewerStopped })
		} else if err != nil {
			log.Printf("AnyKVM: viewer: %v", err)
		}
		close(stopViewer)
	} else {
		if cfg.Viewer {
			log.Print("AnyKVM: viewer support was not compiled; running headless")
		}
		<-viewerStopped
		close(stopViewer)
	}
	log.Println("AnyKVM: shutting down...")
	stopWatch := logging.WatchStall("application shutdown", 5*time.Second)
	defer stopWatch()
	if rdpSrv != nil {
		rdpSrv.Close()
	}
	if gsSrv != nil {
		gsSrv.Close()
	}
	if wsSrv != nil {
		wsSrv.Close()
	}
	b.Stop()
}
