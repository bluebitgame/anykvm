package service

import (
	"sort"
	"strings"
)

const WorkerStopEnvironment = "ANYKVM_SERVICE_STOP_EVENT"
const workerDesktopEnvironment = "ANYKVM_SERVICE_DESKTOP"
const workerExitDefault = 1001
const workerExitWinlogon = 1002

// Windows environment keys are case-insensitive. Replace rather than append
// duplicate PATH entries, and avoid inheriting another worker's control event.
func serviceWorkerEnvironment(base []string, overrides map[string]string, stop, desktop string) []string {
	entries := make(map[string]string)
	for _, entry := range base {
		index := strings.IndexByte(entry, '=')
		if index == 0 { // Windows drive-current-directory variables, e.g. =C:=C:\.
			if next := strings.IndexByte(entry[1:], '='); next >= 0 {
				index = next + 1
			}
		}
		if index > 0 {
			entries[strings.ToUpper(entry[:index])] = entry
		}
	}
	for key, value := range overrides {
		entries[strings.ToUpper(key)] = key + "=" + value
	}
	entries[WorkerStopEnvironment] = WorkerStopEnvironment + "=" + stop
	entries[workerDesktopEnvironment] = workerDesktopEnvironment + "=" + desktop
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, entries[key])
	}
	return result
}

func serviceDesktopExit(desktop string) uint32 {
	switch {
	case strings.EqualFold(desktop, "Default"):
		return workerExitDefault
	case strings.EqualFold(desktop, "Winlogon"):
		return workerExitWinlogon
	default:
		return 0
	}
}
