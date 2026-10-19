# Native capture buffers

Select capture-card format through `targets[].video.mode`, for example
`1920x1080@60:nv12` or `1920x1080@60:mjpeg`. Explicit formats are preserved.
With dimensions/rate but no format, native Windows/Linux video encoding prefers
supported NV12; JPEG consumers prefer supported MJPEG.

## Windows

Video consumers request Media Foundation NV12. Compatible Media Foundation
encoders receive retained native buffers; vendor encoders borrow tightly packed
NV12. Padded samples need row packing, and GPU encoders may still upload system
memory. Rejected native buffers fall back before an input is accepted. Pixel
consumers retain RGB32 capture.

MJPEG is forwarded without JPEG decoding/re-encoding. One single-buffer device
sample can be retained until consumers release it; additional retained samples
copy their compressed data so slow consumers cannot exhaust the device pool.
Multi-buffer samples need one flattening copy. Other VNC encodings may need pixels.

## Linux

Single-planar V4L2 NV12 stays dequeued until synchronous encoding completes.
Compatible Vulkan Video and Raspberry Pi V4L2 encoders can import an exported
DMA-BUF. Vulkan synchronizes foreign queue ownership and completion before
requeue; V4L2 waits for input and output completion.

Unsupported imports fall back to borrowed packed NV12 or one row-packing copy,
without a BGRA conversion. These paths may still upload data inside the encoder.
Multi-planar nodes and vendor tiled layouts are outside this direct path.
MJPEG is copied out once to avoid starving capture and is then shared through
reference-counted ownership without transcoding.

## Hardware validation

Build the `internal/capture` test executable for the target host after
[preparing build inputs](build.md), then run it
on that host with its actual advertised mode, including 59.94 when appropriate.

On Windows, set `ANYKVM_TEST_WINDOWS_CAMERA` to a device name/index and
`ANYKVM_TEST_WINDOWS_CAMERA_MODE=1920x1080@60:nv12`. Run
`capture.test.exe -test.run TestWindowsCameraNV12Direct -test.v`.
`TestWindowsCameraMJPEGPassthrough` selects an advertised MJPEG mode and checks
that retained JPEG data survives capture close.

On Linux, set `ANYKVM_TEST_LINUX_VIDEO=/dev/video0` and
`ANYKVM_TEST_LINUX_VIDEO_MODE=1920x1080@60:nv12`. Run
`./capture.test -test.run TestLinuxV4L2NV12Direct -test.v`. For MJPEG, select a
mode ending in `:mjpeg` and run `TestLinuxV4L2MJPEGPassthrough` instead.

Logs show whether direct import or fallback was used. Test with the target
driver and device to verify import compatibility.
