package videoenc

import "fmt"

// pixelsNV12 fits into an even-sized canvas using limited-range BT.709. The
// native screen path already supplies NV12 directly and bypasses this helper.
func pixelsNV12(data []byte, w, h int, format string, ow, oh int) ([]byte, error) {
	if w < 1 || h < 1 || ow < 2 || oh < 2 || ow%2 != 0 || oh%2 != 0 || w > 8192 || h > 8192 || ow > 8192 || oh > 8192 {
		return nil, fmt.Errorf("invalid pixel dimensions")
	}
	expected := w * h * 4
	switch format {
	case "nv12":
		expected = w*h + 2*((w+1)/2)*((h+1)/2)
	case "yuv420p":
		expected = w*h + 2*((w+1)/2)*((h+1)/2)
	case "uyvy422", "yuyv422":
		expected = w * h * 2
		if w%2 != 0 {
			return nil, fmt.Errorf("odd packed YUV width")
		}
	case "bgra":
	default:
		return nil, fmt.Errorf("unsupported pixels %s", format)
	}
	if len(data) != expected {
		return nil, fmt.Errorf("invalid %s frame length", format)
	}
	if format == "nv12" && w == ow && h == oh {
		return data, nil
	}
	dw, dh := ow, oh
	if w*dh > h*dw {
		dh = (h * dw / w) &^ 1
	} else {
		dw = (w * dh / h) &^ 1
	}
	dw = max(2, dw)
	dh = max(2, dh)
	ox := ((ow - dw) / 2) &^ 1
	oy := ((oh - dh) / 2) &^ 1
	dst := make([]byte, ow*oh*3/2)
	for i := 0; i < ow*oh; i++ {
		dst[i] = 16
	}
	for i := ow * oh; i < len(dst); i++ {
		dst[i] = 128
	}
	sample := func(x, y int) (int, int, int) {
		switch format {
		case "nv12":
			i := w*h + (y/2)*2*((w+1)/2) + (x/2)*2
			return int(data[y*w+x]), int(data[i]), int(data[i+1])
		case "yuv420p":
			cw := (w + 1) / 2
			plane := cw * ((h + 1) / 2)
			i := (y/2)*cw + x/2
			return int(data[y*w+x]), int(data[w*h+i]), int(data[w*h+plane+i])
		case "uyvy422":
			i := (y*w + (x &^ 1)) * 2
			return int(data[i+1+(x%2)*2]), int(data[i]), int(data[i+2])
		case "yuyv422":
			i := (y*w + (x &^ 1)) * 2
			return int(data[i+(x%2)*2]), int(data[i+1]), int(data[i+3])
		default:
			i := (y*w + x) * 4
			b, g, r := int(data[i]), int(data[i+1]), int(data[i+2])
			return ((47*r + 157*g + 16*b + 128) >> 8) + 16, ((-26*r - 87*g + 112*b + 128) >> 8) + 128, ((112*r - 102*g - 10*b + 128) >> 8) + 128
		}
	}
	clamp := func(v int) byte { return byte(min(255, max(0, v))) }
	for y := 0; y < dh; y += 2 {
		for x := 0; x < dw; x += 2 {
			u, v := 0, 0
			for j := 0; j < 2; j++ {
				for i := 0; i < 2; i++ {
					yy, uu, vv := sample((x+i)*w/dw, (y+j)*h/dh)
					dst[(oy+y+j)*ow+ox+x+i] = clamp(yy)
					u += uu
					v += vv
				}
			}
			offset := ow*oh + ((oy+y)/2)*ow + ox + x
			dst[offset] = clamp(u / 4)
			dst[offset+1] = clamp(v / 4)
		}
	}
	return dst, nil
}
