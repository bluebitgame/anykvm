package netstats

import (
	"context"
	"time"

	"anykvm/internal/logging"
)

// Run reports total uplink at debug level every five seconds while remote control is active.
func Run(ctx context.Context, active func() bool) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	last, started := Total(), time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now, total := time.Now(), Total()
			if active != nil && active() {
				logging.Debugf("AnyKVM: uplink=%.1f Mbps", float64(total-last)*8/now.Sub(started).Seconds()/1e6)
			}
			last, started = total, now
		}
	}
}
