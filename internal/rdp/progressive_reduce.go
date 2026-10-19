package rdp

// MS-RDPEGFX 3.2.8.1.2.2. The first level extrapolates sample 64 as
// 2*sample63-sample62, making the final high coefficient zero. Later odd
// levels use ordinary mirroring. Sizes are 33/31, 17/16, then 9/8.
const progressiveReduceLL = 4015

func progressiveReduceLine(input, low, high []int32) {
	for i := range high {
		high[i] = (2*input[2*i+1] - input[2*i] - input[2*i+2] + 2) >> 2
	}
	for i := range low {
		var value, left, right int32
		if 2*i < len(input) {
			value = input[2*i]
		} else {
			value = 2*input[63] - input[62]
		}
		if len(input) == 64 {
			if i <= len(high) {
				left = high[max(0, i-1)]
			}
			if i < len(high) {
				right = high[i]
			}
		} else {
			left = high[min(len(high)-1, max(0, i-1))]
			right = high[min(len(high)-1, i)]
		}
		low[i] = value + ((left + right + 1) >> 1)
	}
}

func progressiveReduceDWT(input []int32) []int32 {
	out := make([]int32, 0, 4096)
	for _, size := range []int{64, 33, 17} {
		n, lo := size/2, (size+1)/2
		if size == 64 {
			n, lo = 31, 33
		}
		vertical, bands := make([]int32, size*size), make([]int32, size*size)
		var column [64]int32
		var low [33]int32
		var high [32]int32
		for x := 0; x < size; x++ {
			for y := 0; y < size; y++ {
				column[y] = input[y*size+x]
			}
			progressiveReduceLine(column[:size], low[:lo], high[:n])
			for y := 0; y < lo; y++ {
				vertical[y*size+x] = low[y]
			}
			for y := 0; y < n; y++ {
				vertical[(lo+y)*size+x] = high[y]
			}
		}
		hl, lh, hh := n*lo, lo*n, n*n
		for y := 0; y < size; y++ {
			progressiveReduceLine(vertical[y*size:(y+1)*size], low[:lo], high[:n])
			if y < lo {
				copy(bands[hl+lh+hh+y*lo:], low[:lo])
				copy(bands[y*n:], high[:n])
			} else {
				copy(bands[hl+(y-lo)*lo:], low[:lo])
				copy(bands[hl+lh+(y-lo)*n:], high[:n])
			}
		}
		out = append(out, bands[:hl+lh+hh]...)
		input = bands[hl+lh+hh:]
	}
	return append(out, input...)
}
