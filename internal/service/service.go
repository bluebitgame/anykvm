// Package service manages desktop startup and supervised Windows sessions.
// Application-specific configuration loading is supplied through Options.Prepare.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config describes the executable, config path, working directory, and inherited environment.
// Its field names also preserve existing Windows service registry serialization.
type Config struct {
	Executable, Config, WorkingDirectory string
	Environment                          map[string]string
}

// Options carries the requested service action and worker lifecycle hooks.
type Options struct {
	Action  string
	Prepare func() (Config, error)
	Stop    <-chan struct{}
	Done    func()
}

func prepareLoginService(opts *Options) (Config, error) {
	if opts.Prepare == nil {
		return Config{}, errors.New("service installation requires a startup configuration provider")
	}
	return opts.Prepare()
}

func runServiceCommand(program string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", program, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// Commit complete definitions by rename, so a failed write never truncates a service.
func writeServiceFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".anykvm-service-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
