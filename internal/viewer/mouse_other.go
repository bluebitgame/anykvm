//go:build viewer && !darwin

package viewer

func platformStartRawMouse() int { return 0 }

func platformStopRawMouse() {}

func platformResyncRawMouse() {}

func platformGCMouseCount() int { return 0 }

func platformSetRawMouseActive(on bool) {}

func platformDrainRawMouse() (dx, dy float32, ok bool) { return 0, 0, false }

func platformStartGCMouseRelay(wake chan struct{}) {}

func platformStopGCMouseRelay() {}
