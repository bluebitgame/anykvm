// Package gpuconv converts Linux DMA-BUF images using Vulkan and shared HLSL.
package gpuconv

import "errors"

var ErrUnavailable = errors.New("Vulkan DMA-BUF conversion unavailable")
