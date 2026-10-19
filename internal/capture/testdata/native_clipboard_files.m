#import <AppKit/AppKit.h>
#include "../clipboard_darwin.h"
#include <assert.h>
#include <fcntl.h>
#include <sys/file.h>
#include <unistd.h>
// Include production code so the fixture can test the internal retirement
// timer with expired timestamps, without touching the general pasteboard.
#include "../clipboard_files_darwin.m"

static NSString *sourcePath;
static NSUInteger downloads;
static BOOL failDownload;

int bk_mainloop_active(void) { return 0; }
int bk_clipboard_materialize(uint64_t offer) {
    (void)offer;
    downloads++;
    if (failDownload) return 0;
    [NSThread sleepForTimeInterval:0.1];
    return [@"complete remote file" writeToFile:sourcePath atomically:NO encoding:NSUTF8StringEncoding error:NULL] ? 1 : 0;
}

static void testLease(BOOL failing, BOOL moving) {
    NSString *directory = [NSTemporaryDirectory() stringByAppendingPathComponent:NSUUID.UUID.UUIDString];
    sourcePath = [directory stringByAppendingPathComponent:@"file.txt"];
    NSFileManager *fm = NSFileManager.defaultManager;
    assert([fm createDirectoryAtPath:directory withIntermediateDirectories:YES attributes:nil error:NULL]);
    assert([fm createFileAtPath:sourcePath contents:[NSData data] attributes:nil]);
    int fd = open([[directory stringByAppendingPathComponent:@".lock"] fileSystemRepresentation], O_CREAT|O_RDWR, 0600);
    assert(fd >= 0 && flock(fd, LOCK_EX|LOCK_NB) == 0);
    NSAutoreleasePool *pool = [[NSAutoreleasePool alloc] init];
    id lease = [[NSClassFromString(@"BKDeferredClipboardLease") alloc] init];
    [lease setValue:directory forKey:@"directory"];
    [lease setValue:@(fd) forKey:@"lockFD"];
    [lease setValue:@1 forKey:@"offer"];
    id<NSFilePresenter> presenter = [[NSClassFromString(@"BKDeferredFilePresenter") alloc] init];
    [(id)presenter setValue:[NSURL fileURLWithPath:sourcePath] forKey:@"presentedItemURL"];
    [(id)presenter setValue:lease forKey:@"lease"];
    [lease release];
    failDownload = failing;
    NSUInteger before = downloads;
    assert([[fm attributesOfItemAtPath:sourcePath error:NULL][NSFileSize] unsignedLongLongValue] == 0);
    __block void (^finished)(void) = nil;
    void (^accessor)(void (^)(void)) = ^(void (^reacquirer)(void)) {
        assert(downloads == before+1);
        if (failing) assert(![fm fileExistsAtPath:sourcePath]);
        else assert([[NSString stringWithContentsOfFile:sourcePath encoding:NSUTF8StringEncoding error:NULL] isEqual:@"complete remote file"]);
        finished = [reacquirer copy];
    };
    if (moving) [presenter relinquishPresentedItemToWriter:accessor];
    else [presenter relinquishPresentedItemToReader:accessor];
    if (!failing) {
        [presenter savePresentedItemChangesWithCompletionHandler:^(NSError *err) { assert(!err); }];
        assert(downloads == before+1); // A second paste reuses complete content.
    }
    NSString *target = nil;
    if (moving) {
        target = [[NSTemporaryDirectory() stringByAppendingPathComponent:NSUUID.UUID.UUIDString] copy];
        assert([fm moveItemAtPath:sourcePath toPath:target error:NULL]);
        [presenter presentedItemDidMoveToURL:[NSURL fileURLWithPath:target]];
    }
    [presenter release];
    [pool drain]; // Atomic property getters can retain through autorelease pools.
    assert([fm fileExistsAtPath:directory]); // A reader owns the temporary files.
    finished();
    [finished release];
    assert(![fm fileExistsAtPath:directory]);
    if (moving) { assert([fm fileExistsAtPath:target]); assert([fm removeItemAtPath:target error:NULL]); [target release]; }
}

static void testRetiredCollection(void) {
    NSString *directory = [NSTemporaryDirectory() stringByAppendingPathComponent:NSUUID.UUID.UUIDString];
    NSFileManager *fm = NSFileManager.defaultManager;
    assert([fm createDirectoryAtPath:directory withIntermediateDirectories:YES attributes:nil error:NULL]);
    @autoreleasepool {
        BKDeferredClipboardLease *lease = [[BKDeferredClipboardLease alloc] init];
        lease.directory = directory;
        lease.lockFD = -1;
        lease.ready = YES;
        BKDeferredFilePresenter *presenter = [[BKDeferredFilePresenter alloc] init];
        presenter.presentedItemURL = [NSURL fileURLWithPath:[directory stringByAppendingPathComponent:@"file"]];
        presenter.lease = lease;
        NSOperationQueue *queue = [[NSOperationQueue alloc] init];
        presenter.presentedItemOperationQueue = queue;
        [queue release];
        [NSFileCoordinator addFilePresenter:presenter];
        if (!BKDeferredOffers) BKDeferredOffers = [[NSMutableDictionary alloc] init];
        BKDeferredOffers[@2] = @[presenter];
        assert(bk_clipboard_file_offer_ready(2) == 1);
        lease.ready = NO;
        assert(bk_clipboard_file_offer_ready(2) == 0);
        lease.ready = YES;
        __block void (^finished)(void) = nil;
        [presenter relinquishPresentedItemToReader:^(void (^reacquirer)(void)) { finished = [reacquirer copy]; }];
        lease.lastAccess = 0;
        retireOfferWhenIdle(2);
        assert(BKDeferredOffers[@2] && [fm fileExistsAtPath:directory]);
        finished();
        [finished release];
        // An idle gap still retains the folder for its later siblings.
        retireOfferWhenIdle(2);
        assert(BKDeferredOffers[@2]);
        lease.lastAccess = 0;
        retireOfferWhenIdle(2);
        assert(!BKDeferredOffers[@2]);
        [presenter release];
        [lease release];
    }
    assert(![fm fileExistsAtPath:directory]);
}

int main(void) {
    @autoreleasepool {
        testLease(NO, NO);
        testLease(YES, NO);
        testLease(NO, YES);
        testRetiredCollection();
        puts("Deferred content, failure, and reader lifetime: PASS");
    }
    return 0;
}
