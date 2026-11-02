package light

import (
	"context"
	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/led"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/metrics"
	"log/slog"
	"math"
	"time"
)

// Multiplying the manual brightness preserves colors and never enables a ring.
func roomScale(lux float64) (float64, bool) {
	if lux < 0 || math.IsNaN(lux) || math.IsInf(lux, 0) {
		return 1, false
	}
	return .20 + .80*min(math.Log1p(lux)/math.Log1p(250), 1), true
}

func settleScale(current, target float64) float64 {
	step := .10 // half-second samples; dim over several seconds
	if target > current {
		step = .35
	}
	return current + step*(target-current)
}

func (l *Light) Run(ctx context.Context) error {
	if !led.HasRing {
		<-ctx.Done()
		return nil
	}
	r := metrics.Reader{}
	path := r.LuxPath() // established safe interfaces, never calibration/debug attrs
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	current := 1.0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			target := 1.0
			enabled := config.Get().Ring.AutoBrightness
			if enabled && path != "" {
				if reading := r.Lux(path); reading.Known {
					if scale, ok := roomScale(reading.Value); ok {
						target = scale
					}
				}
			}
			if enabled {
				current = settleScale(current, target)
			} else {
				current = 1
			}
			if err := led.Get().SetRoomScale(current); err != nil {
				slog.Warn("ring room brightness failed", "err", err)
			}
		}
	}
}
