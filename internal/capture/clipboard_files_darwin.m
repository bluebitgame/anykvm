#import <AppKit/AppKit.h>
#include "clipboard_darwin.h"
#include "mainloop_darwin.h"
#include <fcntl.h>
#include <unistd.h>

static NSString *const BKDeferredClipboardMarker = @"com.anykvm.clipboard.offer";
static NSString *offerMarker(uint64_t offer) {
    return [NSString stringWithFormat:@"%d-%llu", getpid(), (unsigned long long)offer];
}

// A lease is retained by all presenters and every coordinated reader. Removing
// the offer unregisters presenters, but cleanup waits for their last reader.
@interface BKDeferredClipboardLease : NSObject
@property (copy) NSString *directory;
@property int lockFD;
@property uint64_t offer;
@property BOOL ready;
@property BOOL failed;
@property NSUInteger readers;
@property NSTimeInterval lastAccess;
@end
@implementation BKDeferredClipboardLease
- (void)dealloc {
    if (_directory) {
        NSError *error = nil;
        if ([[NSFileManager defaultManager] removeItemAtPath:_directory error:&error])
            NSLog(@"clipboard: removed temporary directory %@", _directory);
        else NSLog(@"clipboard: temporary cleanup %@: %@", _directory, error);
    }
    if (_lockFD >= 0) close(_lockFD);
    [_directory release];
    [super dealloc];
}
@end

@interface BKDeferredFilePresenter : NSObject <NSFilePresenter>
@property (copy) NSURL *presentedItemURL;
@property (retain) NSOperationQueue *presentedItemOperationQueue;
@property (retain) BKDeferredClipboardLease *lease;
@end
@implementation BKDeferredFilePresenter
- (NSError *)ensureContent {
    @synchronized(self.lease) {
        self.lease.lastAccess = NSDate.date.timeIntervalSince1970;
        if (!self.lease.ready && !self.lease.failed) {
            self.lease.ready = bk_clipboard_materialize(self.lease.offer) == 1;
            self.lease.failed = !self.lease.ready;
        }
        if (self.lease.failed)
            return [NSError errorWithDomain:@"AnyKVMClipboard" code:1
                userInfo:@{NSLocalizedDescriptionKey: @"The remote clipboard file could not be downloaded."}];
        return nil;
    }
}
- (void)savePresentedItemChangesWithCompletionHandler:(void (^)(NSError *))completion {
    completion([self ensureContent]);
}
- (void)relinquishPresentedItemToReader:(void (^)(void (^)(void)))reader {
    BKDeferredClipboardLease *lease = self.lease;
    @synchronized(lease) { lease.readers++; }
    NSError *error = [self ensureContent];
    if (error) {
        // This handshake cannot return an error. Make the source unavailable
        // before releasing Finder rather than letting it copy partial content.
        [[NSFileManager defaultManager] removeItemAtURL:self.presentedItemURL error:NULL];
    }
    // Capturing the lease keeps files (and their lifetime lock) alive until the
    // consumer finishes, including after clipboard replacement or disconnect.
    reader(^{
        @synchronized(lease) {
            lease.readers--;
            lease.lastAccess = NSDate.date.timeIntervalSince1970;
        }
    });
}
- (void)relinquishPresentedItemToWriter:(void (^)(void (^)(void)))writer {
    // Finder's Move Item Here must also materialize the source before moving
    // it. The lease only removes the original staging directory, never targets.
    [self relinquishPresentedItemToReader:writer];
}
- (void)presentedItemDidMoveToURL:(NSURL *)url {
    self.presentedItemURL = url;
}
- (void)dealloc {
    [_presentedItemURL release];
    [_presentedItemOperationQueue release];
    [_lease release];
    [super dealloc];
}
@end

static NSMutableDictionary *BKDeferredOffers;

int bk_clipboard_file_offer_ready(uint64_t offer) {
    NSArray *presenters = nil;
    @synchronized([BKDeferredClipboardLease class]) { presenters = [BKDeferredOffers[@(offer)] retain]; }
    // Do not take the lease's materialization mutex on the AppKit thread: the
    // network reader may need that thread to inspect a clipboard revision.
    BOOL ready = presenters.count && [(BKDeferredFilePresenter *)presenters[0] lease].ready;
    [presenters release];
    return ready ? 1 : 0;
}

static void retireOfferNow(uint64_t offer) {
    NSArray *presenters = nil;
    @synchronized([BKDeferredClipboardLease class]) {
        presenters = [BKDeferredOffers[@(offer)] retain];
        [BKDeferredOffers removeObjectForKey:@(offer)];
    }
    for (id presenter in presenters) [NSFileCoordinator removeFilePresenter:presenter];
    [presenters release];
}

// Finder may copy a folder using separate coordinated reads for each child.
// Keep completed retired offers through a quiet period between those reads.
// This avoids deleting later siblings when an earlier file finishes copying.
static void retireOfferWhenIdle(uint64_t offer) {
    @autoreleasepool {
        NSArray *presenters = nil;
        @synchronized([BKDeferredClipboardLease class]) { presenters = [BKDeferredOffers[@(offer)] retain]; }
        if (!presenters.count) { [presenters release]; return; }
        BKDeferredClipboardLease *lease = [(BKDeferredFilePresenter *)presenters[0] lease];
        BOOL busy;
        @synchronized(lease) { busy = lease.readers != 0 || NSDate.date.timeIntervalSince1970-lease.lastAccess < 60; }
        if (busy) dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 60*NSEC_PER_SEC), dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
            retireOfferWhenIdle(offer);
        });
        else retireOfferNow(offer);
        [presenters release];
    }
}

int bk_clipboard_publish_files(const char *json, size_t length, uint64_t offer,
        int lock_fd, uint64_t expected_revision, uint64_t *revision) {
    __block int status = 0;
    void (^publish)(void) = ^{
        @autoreleasepool {
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            *revision = (uint64_t)board.changeCount;
            if (*revision != expected_revision) return;
            NSData *data = [NSData dataWithBytes:json length:length];
            NSDictionary *description = [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL];
            if (![description isKindOfClass:[NSDictionary class]]) return;
            NSArray *roots = description[@"roots"], *paths = description[@"paths"];
            if (![roots isKindOfClass:[NSArray class]] || !roots.count || roots.count > 1024 ||
                ![paths isKindOfClass:[NSArray class]] || !paths.count || paths.count > 2048) return;
            NSMutableArray *urls = [NSMutableArray array];
            for (NSString *path in roots) {
                if (![path isKindOfClass:[NSString class]] || !path.isAbsolutePath) return;
                [urls addObject:[NSURL fileURLWithPath:path]];
            }
            BKDeferredClipboardLease *lease = [[BKDeferredClipboardLease alloc] init];
            lease.lockFD = fcntl(lock_fd, F_DUPFD_CLOEXEC, 0);
            if (lease.lockFD < 0) { [lease release]; return; }
            lease.directory = [roots[0] stringByDeletingLastPathComponent];
            lease.offer = offer;
            lease.ready = [description[@"ready"] boolValue];
            NSMutableArray *presenters = [NSMutableArray array];
            // Finder may coordinate only individual files while enumerating a
            // directory. Register descendants as well as the visible roots.
            for (NSString *path in paths) {
                if (![path isKindOfClass:[NSString class]] || !path.isAbsolutePath ||
                    ![path hasPrefix:[lease.directory stringByAppendingString:@"/"]]) {
                    for (id presenter in presenters) [NSFileCoordinator removeFilePresenter:presenter];
                    [lease release];
                    return;
                }
                NSURL *url = [NSURL fileURLWithPath:path];
                BKDeferredFilePresenter *presenter = [[BKDeferredFilePresenter alloc] init];
                presenter.presentedItemURL = url;
                presenter.lease = lease;
                NSOperationQueue *queue = [[NSOperationQueue alloc] init];
                queue.maxConcurrentOperationCount = 1;
                presenter.presentedItemOperationQueue = queue;
                [queue release];
                [NSFileCoordinator addFilePresenter:presenter];
                [presenters addObject:presenter];
                [presenter release];
            }
            [lease release];
            [board clearContents];
            BOOL written = [board writeObjects:urls];
            if (written) {
                [board addTypes:@[BKDeferredClipboardMarker] owner:nil];
                written = [board setString:offerMarker(offer) forType:BKDeferredClipboardMarker];
            }
            *revision = (uint64_t)board.changeCount;
            if (!written) {
                for (id presenter in presenters) [NSFileCoordinator removeFilePresenter:presenter];
                [board clearContents];
                return;
            }
            @synchronized([BKDeferredClipboardLease class]) {
                if (!BKDeferredOffers) BKDeferredOffers = [[NSMutableDictionary alloc] init];
                BKDeferredOffers[@(offer)] = presenters;
            }
            status = 1;
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), publish);
    else publish();
    return status;
}

void bk_clipboard_retire_files(uint64_t offer) {
    @autoreleasepool {
    NSArray *presenters = nil;
    @synchronized([BKDeferredClipboardLease class]) { presenters = [BKDeferredOffers[@(offer)] retain]; }
    BOOL complete = NO;
    if (presenters.count) {
        BKDeferredClipboardLease *lease = [(BKDeferredFilePresenter *)presenters[0] lease];
        @synchronized(lease) {
            complete = lease.ready;
            lease.lastAccess = NSDate.date.timeIntervalSince1970;
        }
    }
    if (complete) dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 60*NSEC_PER_SEC), dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
        retireOfferWhenIdle(offer);
    });
    else retireOfferNow(offer);
    [presenters release];
    }
}

void bk_clipboard_clear_file_offer(uint64_t offer) {
    void (^clear)(void) = ^{
        @autoreleasepool {
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            NSString *marker = offerMarker(offer);
            if ([[board stringForType:BKDeferredClipboardMarker] isEqual:marker]) [board clearContents];
        }
    };
    // Complete the pasteboard revision change before a protocol snapshots it
    // for a replacement offer. This performs no network or presenter waits.
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), clear);
    else clear();
}

void bk_clipboard_shutdown_files(void) {
    @autoreleasepool {
        NSArray *keys = nil;
        @synchronized([BKDeferredClipboardLease class]) { keys = [[BKDeferredOffers allKeys] copy]; }
        if (!keys.count) { [keys release]; return; }
        for (NSNumber *key in keys) retireOfferNow(key.unsignedLongLongValue);
        [keys release];
        void (^clear)(void) = ^{
            @autoreleasepool {
                NSPasteboard *board = [NSPasteboard generalPasteboard];
                NSString *prefix = [NSString stringWithFormat:@"%d-", getpid()];
                if ([[board stringForType:BKDeferredClipboardMarker] hasPrefix:prefix]) [board clearContents];
            }
        };
        if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), clear);
        else clear();
    }
}
