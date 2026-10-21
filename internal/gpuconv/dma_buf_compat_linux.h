#ifndef ANYKVM_DMA_BUF_COMPAT_LINUX_H
#define ANYKVM_DMA_BUF_COMPAT_LINUX_H

#include <linux/dma-buf.h>
#include <sys/ioctl.h>

// Older build headers lack these Linux UAPI declarations. Keep their ABI
// unchanged; unsupported running kernels still fail the checked ioctl calls.
#ifndef DMA_BUF_IOCTL_EXPORT_SYNC_FILE
struct dma_buf_export_sync_file {
    __u32 flags;
    __s32 fd;
};
#define DMA_BUF_IOCTL_EXPORT_SYNC_FILE _IOWR(DMA_BUF_BASE, 2, struct dma_buf_export_sync_file)
#endif

#ifndef DMA_BUF_IOCTL_IMPORT_SYNC_FILE
struct dma_buf_import_sync_file {
    __u32 flags;
    __s32 fd;
};
#define DMA_BUF_IOCTL_IMPORT_SYNC_FILE _IOW(DMA_BUF_BASE, 3, struct dma_buf_import_sync_file)
#endif

#endif
