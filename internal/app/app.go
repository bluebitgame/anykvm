package app

import (
	"flag"
	"log"
	"os"

	"anykvm/internal/discovery"
	"anykvm/internal/logging"
)

// Run starts AnyKVM using the command-line arguments. The caller must keep
// the original OS main thread locked on macOS and in viewer builds.
func Run() {
	opts := registerServerFlags(flag.CommandLine)
	flag.Parse()
	logging.Configure(opts.info, opts.debug)
	if flag.NArg() != 0 {
		log.Fatal("unexpected arguments; configure services and devices in YAML")
	}
	if err := opts.validateServiceAction(); err != nil {
		log.Fatal(err)
	}
	if opts.serviceAction != "" {
		if err := manageServerService(opts); err != nil {
			log.Fatalf("service: %v", err)
		}
		return
	}
	if handled, err := runManagedServer(opts); err != nil {
		log.Fatalf("service runtime: %v", err)
	} else if handled {
		return
	}
	if opts.serviceDone != nil {
		defer opts.serviceDone()
	}
	runConfiguredServer(opts)
}

func runConfiguredServer(opts *serverOptions) {

	if opts.listDevices {
		discovery.List(opts.listIOS)
		return
	}

	if opts.pairSpec != "" {
		runPair(opts.pairSpec, effectiveMouseMode(opts.pairMouseMode))
		return
	}

	if opts.genConfig != "" {
		if err := SaveDefaultConfig(opts.genConfig); err != nil {
			log.Fatalf("generate config: %v", err)
		}
		log.Printf("default config written to %s", opts.genConfig)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		log.Fatalf("resolve executable for config: %v", err)
	}
	configPath, err := serverConfigPath(opts.configPath, executable)
	if err != nil {
		log.Fatalf("resolve config: %v", err)
	}
	cfg, created, err := loadServerConfig(configPath, opts.configPath == "")
	if err != nil {
		log.Fatalf("load config %s: %v", configPath, err)
	}
	if created {
		log.Printf("AnyKVM: initialized %s with password-free Web on 127.0.0.1:8080", configPath)
	} else {
		log.Printf("AnyKVM: loaded config %s", configPath)
	}
	runApplicationMain(cfg, func() { runConfiguredApplication(opts, configPath, cfg) })
}
