package netstats

import (
	"context"
	"log"
	"time"
)

// Run reports total uplink every five seconds while remote control is active.
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
				log.Printf("AnyKVM: uplink=%.1f Mbps", float64(total-last)*8/now.Sub(started).Seconds()/1e6)
			}
			last, started = total, now
		}
	}
}
