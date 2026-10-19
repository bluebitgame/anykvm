//go:build windows

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const windowsServiceName = "AnyKVM"
const windowsServiceKey = `SYSTEM\CurrentControlSet\Services\AnyKVM`

func stopWindowsService(service *mgr.Service) error {
	status, err := service.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Stopped {
		return nil
	}
	if status.State != svc.StopPending {
		if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err = service.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timed out stopping AnyKVM service")
}

func Manage(opts *Options) (resultErr error) {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows service manager (use an Administrator terminal): %w", err)
	}
	defer m.Disconnect()
	service, openErr := m.OpenService(windowsServiceName)
	if openErr != nil && !errors.Is(openErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return openErr
	}
	if service != nil {
		defer service.Close()
	}
	if opts.Action == "install" {
		cfg, err := prepareLoginService(opts)
		if err != nil {
			return err
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		var oldConfig mgr.Config
		var oldStartup string
		var wasRunning bool
		created := service == nil
		if !created {
			oldConfig, err = service.Config()
			if err != nil {
				return err
			}
			status, err := service.Query()
			if err != nil {
				return err
			}
			wasRunning = status.State != svc.Stopped
			key, err := registry.OpenKey(registry.LOCAL_MACHINE, windowsServiceKey, registry.QUERY_VALUE)
			if err != nil {
				return err
			}
			oldStartup, _, err = key.GetStringValue("AnyKVMStartup")
			key.Close()
			if err != nil {
				return err
			}
		}
		definition := mgr.Config{
			DisplayName: "AnyKVM server", Description: "AnyKVM console-session capture and remote input",
			StartType: mgr.StartAutomatic, ServiceStartName: "LocalSystem",
			BinaryPathName: windows.ComposeCommandLine([]string{cfg.Executable, "-config", cfg.Config}),
		}
		if created {
			service, err = m.CreateService(windowsServiceName, cfg.Executable, definition, "-config", cfg.Config)
			if err != nil {
				return fmt.Errorf("install service (use an Administrator terminal): %w", err)
			}
			defer service.Close()
		} else {
			if err := stopWindowsService(service); err != nil {
				return err
			}
		}
		defer func() {
			if resultErr == nil {
				return
			}
			if created {
				resultErr = errors.Join(resultErr, service.Delete())
				return
			}
			resultErr = errors.Join(resultErr, service.UpdateConfig(oldConfig))
			key, restoreErr := registry.OpenKey(registry.LOCAL_MACHINE, windowsServiceKey, registry.SET_VALUE)
			if restoreErr == nil {
				restoreErr = key.SetStringValue("AnyKVMStartup", oldStartup)
				key.Close()
			}
			resultErr = errors.Join(resultErr, restoreErr)
			if wasRunning {
				resultErr = errors.Join(resultErr, service.Start())
			}
		}()
		if !created {
			if err := service.UpdateConfig(definition); err != nil {
				return err
			}
		}
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, windowsServiceKey, registry.SET_VALUE)
		if err != nil {
			return err
		}
		err = key.SetStringValue("AnyKVMStartup", string(data))
		key.Close()
		if err != nil {
			return err
		}
		if err := service.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 10 * time.Second}}, 86400); err != nil {
			return err
		}
		if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
			return err
		}
		if err := service.Start(); err != nil {
			return err
		}
		log.Print("AnyKVM: automatic Windows service installed and started; worker follows the active console session as SYSTEM")
		return nil
	}
	if service == nil {
		if opts.Action == "uninstall" {
			log.Print("AnyKVM: service is not installed")
			return nil
		}
		return errors.New("AnyKVM service is not installed")
	}
	switch opts.Action {
	case "uninstall":
		if err := stopWindowsService(service); err != nil {
			return err
		}
		if err := service.Delete(); err != nil {
			return err
		}
		log.Print("AnyKVM: service uninstalled; configuration and logs retained")
	case "status":
		status, err := service.Query()
		if err != nil {
			return err
		}
		states := map[svc.State]string{svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping", svc.Running: "running", svc.Paused: "paused"}
		fmt.Printf("AnyKVM: state=%s, process=%d\n", states[status.State], status.ProcessId)
	case "restart":
		if err := stopWindowsService(service); err != nil {
			return err
		}
		return service.Start()
	}
	return nil
}

func Run(opts *Options) (bool, error) {
	managed, err := svc.IsWindowsService()
	if err != nil {
		return false, err
	}
	if !managed {
		return false, prepareWindowsWorker(opts)
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, windowsServiceKey, registry.QUERY_VALUE)
	if err != nil {
		return true, err
	}
	data, _, err := key.GetStringValue("AnyKVMStartup")
	key.Close()
	if err != nil {
		return true, err
	}
	var cfg Config
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		return true, err
	}
	if !filepath.IsAbs(cfg.Executable) || !filepath.IsAbs(cfg.Config) || !filepath.IsAbs(cfg.WorkingDirectory) {
		return true, errors.New("service startup paths must be absolute; reinstall the service")
	}
	logs := filepath.Join(os.Getenv("ProgramData"), "AnyKVM", "logs")
	if !filepath.IsAbs(logs) {
		return true, errors.New("ProgramData is unavailable")
	}
	if err := os.MkdirAll(logs, 0755); err != nil {
		return true, err
	}
	file, err := os.OpenFile(filepath.Join(logs, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return true, err
	}
	defer file.Close()
	log.SetOutput(file)
	defer log.SetOutput(os.Stderr)
	return true, svc.Run(windowsServiceName, &windowsServiceSupervisor{cfg: cfg, logFile: file})
}
