package app

import (
	"fmt"
	"os"
	"path/filepath"

	"anykvm/internal/service"
)

func prepareLoginService(opts *serverOptions) (service.Config, error) {
	executable, err := os.Executable()
	if err != nil {
		return service.Config{}, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return service.Config{}, err
	}
	return resolveLoginService(opts.configPath, executable, wd)
}

func resolveLoginService(config, executable, wd string) (service.Config, error) {
	implicit := config == ""
	if !implicit && !filepath.IsAbs(config) {
		config = filepath.Join(wd, config)
	}
	path, err := serverConfigPath(config, executable)
	if err != nil {
		return service.Config{}, err
	}
	if _, _, err := loadServerConfig(path, implicit); err != nil {
		return service.Config{}, fmt.Errorf("load config %s: %w", path, err)
	}
	environment := make(map[string]string)
	for _, key := range []string{"PATH", "ANYKVM_FFMPEG_LIBDIR", "SCRCPY_SERVER_PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			environment[key] = value
		}
	}
	return service.Config{executable, path, wd, environment}, nil
}

func manageServerService(opts *serverOptions) error {
	return service.Manage(&service.Options{Action: opts.serviceAction, Prepare: func() (service.Config, error) { return prepareLoginService(opts) }})
}

func runManagedServer(opts *serverOptions) (bool, error) {
	runtime := &service.Options{}
	handled, err := service.Run(runtime)
	opts.serviceStop, opts.serviceDone = runtime.Stop, runtime.Done
	return handled, err
}
