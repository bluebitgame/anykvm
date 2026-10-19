// Package framerate represents video rates without floating-point rounding.
package framerate

import "time"

// Rate is a frame rate in frames per second. The zero value means unknown.
type Rate struct {
	Num int
	Den int
}

func New(num, den int) Rate {
	if num <= 0 || den <= 0 || num > 2147483647 || den > 2147483647 {
		return Rate{}
	}
	a, b := num, den
	for b != 0 {
		a, b = b, a%b
	}
	return Rate{num / a, den / a}
}
func (r Rate) Valid() bool {
	return r.Num > 0 && r.Den > 0 && r.Num <= 2147483647 && r.Den <= 2147483647
}
func (r Rate) Round() int {
	if !r.Valid() {
		return 0
	}
	return int((int64(r.Num) + int64(r.Den)/2) / int64(r.Den))
}
func (r Rate) Interval() time.Duration {
	if !r.Valid() {
		return 0
	}
	return time.Duration(int64(time.Second) * int64(r.Den) / int64(r.Num))
}

// Or returns the normalized rate, falling back to an integer rate if unknown.
func (r Rate) Or(fps int) Rate {
	if r.Valid() {
		return New(r.Num, r.Den)
	}
	return New(fps, 1)
}

// Decimate only removes frames at an integer interval. A 0.1% tolerance allows
// NTSC rates to match integer requests. The returned rate preserves the cadence.
func (r Rate) Decimate(requested int) (Rate, int) {
	r = New(r.Num, r.Den)
	if !r.Valid() || requested <= 0 || requested > 240 {
		return r, 1
	}
	n, d := int64(r.Num), int64(r.Den)*int64(requested)
	divisor := (n + d/2) / d
	if divisor < 2 {
		return r, 1
	}
	target := d * divisor
	diff := n - target
	if diff < 0 {
		diff = -diff
	}
	if diff*1000 > target {
		return r, 1
	}
	// Reduce before multiplying the denominator to keep both fields in range.
	a, b := int64(r.Num), divisor
	for b != 0 {
		a, b = b, a%b
	}
	return New(int(int64(r.Num)/a), int(int64(r.Den)*(divisor/a))), int(divisor)
}
