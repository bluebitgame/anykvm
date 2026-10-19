//go:build windows

package service

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

type windowsServiceSupervisor struct {
	cfg     Config
	logFile *os.File
}

type windowsServiceChild struct {
	process, job, stop windows.Handle
	session            uint32
}

func enableServicePrivileges() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	for _, name := range []string{"SeTcbPrivilege", "SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"} {
		ptr, _ := windows.UTF16PtrFromString(name)
		state := windows.Tokenprivileges{PrivilegeCount: 1}
		if err := windows.LookupPrivilegeValue(nil, ptr, &state.Privileges[0].Luid); err != nil {
			return err
		}
		state.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
		if err := windows.AdjustTokenPrivileges(token, false, &state, 0, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *windowsServiceSupervisor) startChild(session uint32, desktop string) (child *windowsServiceChild, err error) {
	var source, token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE, &source); err != nil {
		return nil, err
	}
	defer source.Close()
	if err := windows.DuplicateTokenEx(source, windows.TOKEN_ALL_ACCESS, nil, windows.SecurityImpersonation, windows.TokenPrimary, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	if err := windows.SetTokenInformation(token, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), 4); err != nil {
		return nil, err
	}
	child = &windowsServiceChild{session: session}
	defer func() {
		if err != nil {
			child.close()
		}
	}()
	child.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return child, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(child.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return child, err
	}
	stopName := fmt.Sprintf(`Global\AnyKVM-stop-%d-%d`, os.Getpid(), time.Now().UnixNano())
	stopPtr, _ := windows.UTF16PtrFromString(stopName)
	child.stop, err = windows.CreateEvent(nil, 1, 0, stopPtr)
	if err != nil {
		return child, err
	}
	logHandle := windows.Handle(s.logFile.Fd())
	if err = windows.SetHandleInformation(logHandle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return child, err
	}
	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return child, err
	}
	defer attributes.Delete()
	if err = attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&logHandle), unsafe.Sizeof(logHandle)); err != nil {
		return child, err
	}
	// PROC_THREAD_ATTRIBUTE_JOB_LIST associates the job atomically at creation.
	// There is no interval in which a supervisor crash can orphan the worker.
	if err = attributes.Update(0x0002000d, unsafe.Pointer(&child.job), unsafe.Sizeof(child.job)); err != nil {
		return child, err
	}
	startup := windows.StartupInfoEx{}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Desktop, err = windows.UTF16PtrFromString(`winsta0\` + desktop)
	if err != nil {
		return child, err
	}
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.StdOutput, startup.StdErr = logHandle, logHandle
	startup.ProcThreadAttributeList = attributes.List()
	app, err := windows.UTF16PtrFromString(s.cfg.Executable)
	if err != nil {
		return child, err
	}
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{s.cfg.Executable, "-config", s.cfg.Config}))
	if err != nil {
		return child, err
	}
	wd, err := windows.UTF16PtrFromString(s.cfg.WorkingDirectory)
	if err != nil {
		return child, err
	}
	environment := serviceWorkerEnvironment(os.Environ(), s.cfg.Environment, stopName, desktop)
	block := utf16.Encode([]rune(strings.Join(environment, "\x00") + "\x00\x00"))
	var process windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err = windows.CreateProcessAsUser(token, app, command, nil, nil, true, flags, &block[0], wd, &startup.StartupInfo, &process); err != nil {
		return child, err
	}
	child.process = process.Process
	defer windows.CloseHandle(process.Thread)
	log.Printf("AnyKVM: worker %d started in console session %d, desktop %s", process.ProcessId, session, desktop)
	return child, nil
}

func (c *windowsServiceChild) close() {
	if c == nil {
		return
	}
	if c.job != 0 {
		windows.CloseHandle(c.job)
		c.job = 0
	}
	if c.process != 0 {
		windows.CloseHandle(c.process)
		c.process = 0
	}
	if c.stop != 0 {
		windows.CloseHandle(c.stop)
		c.stop = 0
	}
}

func (c *windowsServiceChild) stopAndClose() {
	if c == nil {
		return
	}
	if err := windows.SetEvent(c.stop); err != nil {
		log.Printf("signal worker shutdown: %v", err)
	}
	result, err := windows.WaitForSingleObject(c.process, 20000)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		log.Print("AnyKVM: worker shutdown timed out; terminating its job")
	}
	c.close()
}

func (s *windowsServiceSupervisor) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	if err := enableServicePrivileges(); err != nil {
		log.Printf("service privileges: %v", err)
		return true, 1
	}
	running := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptSessionChange}
	statuses <- running
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var child *windowsServiceChild
	defer func() { child.stopAndClose() }()
	desktop := "Default"
	nextStart := time.Time{}
	for {
		select {
		case request, ok := <-requests:
			if !ok {
				return false, 0
			}
			switch request.Cmd {
			case svc.Stop, svc.Shutdown:
				statuses <- svc.Status{State: svc.StopPending, WaitHint: 25000}
				return false, 0
			case svc.Interrogate:
				statuses <- running
			}
		case <-ticker.C:
		}
		session := windows.WTSGetActiveConsoleSessionId()
		if child != nil && child.session != session {
			log.Print("AnyKVM: console session changed; restarting worker")
			child.stopAndClose()
			child = nil
			desktop = "Default"
			nextStart = time.Time{}
		}
		if child != nil {
			var code uint32
			if err := windows.GetExitCodeProcess(child.process, &code); err != nil {
				log.Printf("worker status: %v", err)
				return true, 1
			}
			if code == 259 {
				continue
			} // STILL_ACTIVE
			log.Printf("AnyKVM: worker exited with code %d", code)
			child.close()
			child = nil
			switch code {
			case workerExitDefault:
				desktop = "Default"
				nextStart = time.Time{}
			case workerExitWinlogon:
				desktop = "Winlogon"
				nextStart = time.Time{}
			default:
				nextStart = time.Now().Add(10 * time.Second)
			}
		}
		if session == 0xffffffff || time.Now().Before(nextStart) {
			continue
		}
		var err error
		child, err = s.startChild(session, desktop)
		if err != nil {
			log.Printf("start console worker: %v", err)
			child = nil
			nextStart = time.Now().Add(10 * time.Second)
		}
	}
}
