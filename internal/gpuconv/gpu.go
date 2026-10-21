// Package gpuconv provides GPU image conversion and Progressive compute runtimes.
package gpuconv

import "errors"

var ErrUnavailable = errors.New("Vulkan DMA-BUF conversion unavailable")
