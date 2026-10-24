package capture

import "math/bits"

// qpcPresentationPTS returns microseconds relative to QPC's arbitrary origin,
// without a Go/wall-clock anchor. Keeping this origin across capture reopen and
// codec/target changes avoids resetting a live browser's playback timeline.
// Recovery pictures repeat old pixels but need a fresh acquisition timestamp.
func qpcPresentationPTS(present, acquired, frequency int64, repeated bool) int64 {
	if acquired <= 0 || frequency <= 0 {
		return 0
	}
	ticks := present
	if repeated || ticks <= 0 || ticks > acquired {
		ticks = acquired
	}
	const micros = 1_000_000
	const maxPTS = 1<<53 - 1 // exactly representable by browser Number values
	seconds, remainder := ticks/frequency, ticks%frequency
	if seconds > maxPTS/micros {
		return 0
	}
	// Multiply only the fractional remainder, using a wide intermediate so
	// even unusual frequencies and long system uptimes cannot overflow.
	hi, lo := bits.Mul64(uint64(remainder), micros)
	fraction, _ := bits.Div64(hi, lo, uint64(frequency))
	pts := seconds*micros + int64(fraction)
	if pts <= 0 || pts > maxPTS {
		return 0
	}
	return pts
}
