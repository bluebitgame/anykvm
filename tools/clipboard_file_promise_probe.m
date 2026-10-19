// Standalone native file-promise probe.
// Build: clang -fblocks -c internal/capture/clipboard_files_darwin.m -o /private/tmp/anykvm-native-clipboard.o
//   clang -fobjc-arc -framework AppKit -framework ApplicationServices \
//   tools/clipboard_file_promise_probe.m /private/tmp/anykvm-native-clipboard.o -o /private/tmp/anykvm-file-promise-probe
// Check: /private/tmp/anykvm-file-promise-probe --self-test
// Finder: /private/tmp/anykvm-file-promise-probe --publish appkit 120
//         /private/tmp/anykvm-file-promise-probe --publish carbon 120
// Control: --publish local-file 120; delayed URL: --publish file-url 120.
// Coordinated placeholder: --publish presenter-file 120.
// Unfilled placeholder for destination tracking: --publish presenter-empty 120.
// Paste into a disposable Finder folder while publication is active.
// Original clipboard data is restored only if no newer copy replaces this offer.
// Run outside a sandbox that blocks macOS application services. A private
// AppKit receiver can time out without a drag session; this is inconclusive.
#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>
#include <sys/file.h>
#include "../internal/capture/clipboard_darwin.h"

// These modes link the production presenter rather than a probe implementation.
static NSString *nativeRoot;
static NSUInteger nativeWrites;
static BOOL nativeFail;
int bk_mainloop_active(void) { return 0; }
int bk_clipboard_materialize(uint64_t offer) {
    (void)offer;
    fprintf(stdout, "%.3f NATIVE download requested; mainThread=%d\n", NSDate.date.timeIntervalSince1970, NSThread.isMainThread);
    fflush(stdout);
    [NSThread sleepForTimeInterval:2];
    if (nativeFail) {
        [@"partial" writeToFile:nativeRoot atomically:NO encoding:NSUTF8StringEncoding error:NULL];
        fprintf(stdout, "%.3f NATIVE simulated failure after partial content\n", NSDate.date.timeIntervalSince1970);
        fflush(stdout);
        return 0;
    }
    NSString *path = nativeRoot;
    BOOL directory = NO;
    if ([NSFileManager.defaultManager fileExistsAtPath:path isDirectory:&directory] && directory)
        path = [path stringByAppendingPathComponent:@"nested/报告.txt"];
    BOOL ok = [@"AnyKVM native file promise: generated only when requested.\n"
        writeToFile:path atomically:NO encoding:NSUTF8StringEncoding error:NULL];
    if (ok) nativeWrites++;
    fprintf(stdout, "%.3f NATIVE content ready=%d\n", NSDate.date.timeIntervalSince1970, ok);
    fflush(stdout);
    return ok ? 1 : 0;
}

static NSString *const filename = @"anykvm-promise-probe.txt";
static NSData *payload(void) {
    return [@"AnyKVM native file promise: generated only when requested.\n"
        dataUsingEncoding:NSUTF8StringEncoding];
}
static void report(NSString *message) {
    fprintf(stdout, "%.3f %s\n", NSDate.date.timeIntervalSince1970, message.UTF8String);
    fflush(stdout);
}
static NSError *writeFile(NSURL *url) {
    // Do not overwrite an existing user file or use a staging directory.
    int fd = open(url.fileSystemRepresentation, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0600);
    if (fd < 0) return [NSError errorWithDomain:NSPOSIXErrorDomain code:errno userInfo:nil];
    NSData *data = payload();
    ssize_t size = write(fd, data.bytes, data.length);
    int saved = errno;
    if (close(fd) < 0 && size == (ssize_t)data.length) { size = -1; saved = errno; }
    if (size != (ssize_t)data.length) {
        unlink(url.fileSystemRepresentation);
        return [NSError errorWithDomain:NSPOSIXErrorDomain code:saved ?: EIO userInfo:nil];
    }
    return nil;
}

@interface ProbeProvider : NSObject <NSFilePromiseProviderDelegate, NSPasteboardItemDataProvider>
@property NSOperationQueue *queue;
@property NSUInteger writes;
@property NSURL *destination;
@property NSURL *sourceURL;
@end
@implementation ProbeProvider
- (instancetype)init {
    if ((self = [super init])) {
        _queue = [NSOperationQueue new];
        _queue.maxConcurrentOperationCount = 1;
    }
    return self;
}
- (NSString *)filePromiseProvider:(NSFilePromiseProvider *)provider fileNameForType:(NSString *)type {
    report(@"APPKIT filename requested (no file content written)");
    return filename;
}
- (NSOperationQueue *)operationQueueForFilePromiseProvider:(NSFilePromiseProvider *)provider {
    return self.queue;
}
- (void)filePromiseProvider:(NSFilePromiseProvider *)provider writePromiseToURL:(NSURL *)url
        completionHandler:(void (^)(NSError *))completion {
    self.writes++;
    self.destination = url;
    report([NSString stringWithFormat:@"APPKIT write requested: %@; mainThread=%d", url.path, NSThread.isMainThread]);
    NSError *error = writeFile(url);
    report(error ? [NSString stringWithFormat:@"APPKIT failed: %@", error] : @"APPKIT completed");
    completion(error);
}
- (void)pasteboard:(NSPasteboard *)board item:(NSPasteboardItem *)item provideDataForType:(NSPasteboardType)type {
    PasteboardRef ref = NULL;
    CFURLRef location = NULL;
    OSStatus status = PasteboardCreate(kPasteboardClipboard, &ref);
    if (!status) status = PasteboardCopyPasteLocation(ref, &location);
    NSURL *directory = CFBridgingRelease(location);
    report([NSString stringWithFormat:@"FILE-URL data requested: destination=%@ status=%d", directory.path ?: @"NONE", (int)status]);
    if (ref) CFRelease(ref);
    if (![NSFileManager.defaultManager fileExistsAtPath:self.sourceURL.path]) {
        NSError *error = writeFile(self.sourceURL);
        if (error) { report(error.description); return; }
        self.writes++;
    }
    [item setString:self.sourceURL.absoluteString forType:type];
}
@end

@interface PlaceholderPresenter : NSObject <NSFilePresenter>
@property (copy) NSURL *presentedItemURL;
@property NSOperationQueue *presentedItemOperationQueue;
@property NSUInteger writes;
@property BOOL leaveEmpty;
@end
@implementation PlaceholderPresenter
- (instancetype)init {
    if ((self = [super init])) {
        _presentedItemOperationQueue = [NSOperationQueue new];
        _presentedItemOperationQueue.maxConcurrentOperationCount = 1;
    }
    return self;
}
- (NSError *)materialize {
    if (self.leaveEmpty) { report(@"PRESENTER leaving source empty for destination tracking"); return nil; }
    report([NSString stringWithFormat:@"PRESENTER materialize requested: %@; mainThread=%d writes=%lu",
        self.presentedItemURL.path, NSThread.isMainThread, (unsigned long)self.writes]);
    NSError *error = nil;
    if (!self.writes) {
        // Simulate a slow transfer while Finder's coordinated read is pending.
        // Only this probe owns the placeholder; never create or replace another file.
        [NSThread sleepForTimeInterval:2];
        int fd = open(self.presentedItemURL.fileSystemRepresentation, O_WRONLY | O_NOFOLLOW);
        if (fd < 0) error = [NSError errorWithDomain:NSPOSIXErrorDomain code:errno userInfo:nil];
        else {
            NSData *data = payload();
            ssize_t count = write(fd, data.bytes, data.length);
            int saved = errno;
            if (close(fd) < 0 && count == (ssize_t)data.length) { count = -1; saved = errno; }
            if (count != (ssize_t)data.length)
                error = [NSError errorWithDomain:NSPOSIXErrorDomain code:saved ?: EIO userInfo:nil];
            else self.writes++;
        }
    }
    report(error ? [NSString stringWithFormat:@"PRESENTER save failed: %@", error] : @"PRESENTER content ready");
    return error;
}
- (void)savePresentedItemChangesWithCompletionHandler:(void (^)(NSError *))completion {
    report(@"PRESENTER save requested");
    completion([self materialize]);
}
- (void)relinquishPresentedItemToReader:(void (^)(void (^)(void)))reader {
    report(@"PRESENTER relinquish to reader");
    PasteboardRef board = NULL;
    CFURLRef location = NULL;
    OSStatus status = PasteboardCreate(kPasteboardClipboard, &board);
    if (!status) status = PasteboardCopyPasteLocation(board, &location);
    NSURL *directory = CFBridgingRelease(location);
    report([NSString stringWithFormat:@"PRESENTER paste location: %@ status=%d",
        directory.path ?: @"NONE", (int)status]);
    if (board) CFRelease(board);
    // Unlike the save callback, this handshake cannot return an NSError to
    // the reader. Production use must separately handle transfer failures.
    [self materialize];
    reader(^{ report(@"PRESENTER reader finished"); });
}
@end

typedef struct { NSUInteger calls; } CarbonState;
static OSStatus refusePromise(PasteboardRef board, PasteboardItemID item, CFStringRef type, void *context) {
    (void)board; (void)item; (void)type; (void)context;
    return badPasteboardFlavorErr;
}
static OSStatus keepPromise(PasteboardRef board, PasteboardItemID item, CFStringRef type, void *context) {
    CarbonState *state = context;
    report([NSString stringWithFormat:@"CARBON requested type: %@", (__bridge NSString *)type]);
    if (!CFEqual(type, kPasteboardTypeFileURLPromise)) return badPasteboardFlavorErr;
    CFURLRef location = NULL;
    OSStatus status = PasteboardCopyPasteLocation(board, &location);
    if (status != noErr || !location) {
        report([NSString stringWithFormat:@"CARBON missing destination: %d", (int)status]);
        return status ?: badPasteboardFlavorErr;
    }
    NSURL *directory = CFBridgingRelease(location);
    if (!directory.isFileURL) return paramErr;
    NSURL *url = [directory URLByAppendingPathComponent:filename];
    state->calls++;
    report([NSString stringWithFormat:@"CARBON write requested: %@; mainThread=%d", url.path, NSThread.isMainThread]);
    NSError *error = writeFile(url);
    if (error) { report(error.description); return ioErr; }
    NSData *data = [url.absoluteString dataUsingEncoding:NSUTF8StringEncoding];
    status = PasteboardPutItemFlavor(board, item, kPasteboardTypeFileURLPromise, (__bridge CFDataRef)data, 0);
    report([NSString stringWithFormat:@"CARBON completed: %d", (int)status]);
    return status;
}
static OSStatus publishCarbon(PasteboardRef board, CarbonState *state) {
    OSStatus status = PasteboardClear(board);
    if (!status) status = PasteboardSetPromiseKeeper(board, keepPromise, state);
    NSData *content = [@"public.plain-text" dataUsingEncoding:NSUTF8StringEncoding];
    if (!status) status = PasteboardPutItemFlavor(board, (PasteboardItemID)1,
        kPasteboardTypeFilePromiseContent, (__bridge CFDataRef)content, 0);
    if (!status) status = PasteboardPutItemFlavor(board, (PasteboardItemID)1,
        kPasteboardTypeFileURLPromise, kPasteboardPromisedData, 0);
    return status;
}
static void pump(NSTimeInterval seconds) {
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:seconds];
    while (deadline.timeIntervalSinceNow > 0) {
        @autoreleasepool {
            NSEvent *event = [NSApp nextEventMatchingMask:NSEventMaskAny
                untilDate:[NSDate dateWithTimeIntervalSinceNow:0.05]
                inMode:NSDefaultRunLoopMode dequeue:YES];
            if (event) [NSApp sendEvent:event];
        }
    }
}

static int selfTest(void) {
    NSString *root = [NSTemporaryDirectory() stringByAppendingPathComponent:NSUUID.UUID.UUIDString];
    NSFileManager *fm = NSFileManager.defaultManager;
    NSError *error = nil;
    if (![fm createDirectoryAtPath:root withIntermediateDirectories:YES attributes:nil error:&error]) {
        report(error.description); return 1;
    }
    NSURL *appkitDir = [NSURL fileURLWithPath:[root stringByAppendingPathComponent:@"appkit"] isDirectory:YES];
    NSURL *carbonDir = [NSURL fileURLWithPath:[root stringByAppendingPathComponent:@"carbon"] isDirectory:YES];
    [fm createDirectoryAtURL:appkitDir withIntermediateDirectories:YES attributes:nil error:&error];
    [fm createDirectoryAtURL:carbonDir withIntermediateDirectories:YES attributes:nil error:&error];
    NSPasteboard *board = [NSPasteboard pasteboardWithUniqueName];
    ProbeProvider *delegate = [ProbeProvider new];
    NSFilePromiseProvider *provider = [[NSFilePromiseProvider alloc] initWithFileType:@"public.plain-text" delegate:delegate];
    BOOL offered = [board writeObjects:@[provider]];
    NSArray *receivers = [board readObjectsForClasses:@[NSFilePromiseReceiver.class] options:nil];
    report([NSString stringWithFormat:@"APPKIT offered=%d receivers=%lu writesBeforeReceive=%lu types=%@",
        offered, (unsigned long)receivers.count, (unsigned long)delegate.writes, board.types]);
    __block BOOL completed = NO;
    __block BOOL passed = NO;
    NSObject *completionLock = [NSObject new];
    // Receiving waits for the provider; sharing its serial queue would deadlock.
    NSOperationQueue *receiverQueue = [NSOperationQueue new];
    BOOL lazy = delegate.writes == 0;
    for (NSFilePromiseReceiver *receiver in receivers) {
        [receiver receivePromisedFilesAtDestination:appkitDir options:@{} operationQueue:receiverQueue
            reader:^(NSURL *url, NSError *err) {
                @synchronized(completionLock) {
                    passed = !err && [[NSData dataWithContentsOfURL:url] isEqualToData:payload()];
                    completed = YES;
                }
            }];
    }
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:10];
    while (deadline.timeIntervalSinceNow > 0) {
        @synchronized(completionLock) { if (completed) break; }
        pump(0.05);
    }
    BOOL appkitOK;
    @synchronized(completionLock) {
        appkitOK = offered && lazy && completed && passed && delegate.writes == 1 &&
            [delegate.destination.URLByDeletingLastPathComponent.path isEqual:appkitDir.path];
    }
    [board clearContents];
    [board releaseGlobally];

    PasteboardRef carbon = NULL;
    CarbonState state = {0};
    NSString *name = [@"anykvm.probe." stringByAppendingString:NSUUID.UUID.UUIDString];
    OSStatus status = PasteboardCreate((__bridge CFStringRef)name, &carbon);
    if (!status) status = publishCarbon(carbon, &state);
    BOOL carbonLazy = state.calls == 0;
    if (!status) status = PasteboardSetPasteLocation(carbon, (__bridge CFURLRef)carbonDir);
    CFDataRef result = NULL;
    if (!status) status = PasteboardCopyItemFlavorData(carbon, (PasteboardItemID)1, kPasteboardTypeFileURLPromise, &result);
    BOOL carbonOK = !status && carbonLazy && state.calls == 1 &&
        [[NSData dataWithContentsOfURL:[carbonDir URLByAppendingPathComponent:filename]] isEqualToData:payload()];
    if (result) CFRelease(result);
    if (carbon) { PasteboardClear(carbon); CFRelease(carbon); }
    [fm removeItemAtPath:root error:nil];
    NSString *appkitResult = !completed ? @"INCONCLUSIVE (receiver timed out)" : appkitOK ? @"PASS" : @"FAIL";
    report([NSString stringWithFormat:@"SELF-TEST appkit=%@ carbon=%@ (receiver simulation; Finder remains unverified)",
        appkitResult, carbonOK ? @"PASS" : @"FAIL"]);
    return carbonOK && (!completed || appkitOK) ? 0 : 1;
}

static int publish(NSString *mode, double duration) {
    NSPasteboard *board = NSPasteboard.generalPasteboard;
    NSMutableArray *saved = [NSMutableArray array];
    for (NSPasteboardItem *item in board.pasteboardItems) {
        for (NSString *type in item.types) {
            if ([type containsString:@"promise"]) { report(@"Existing file promise: copy plain text before this probe."); return 1; }
        }
    }
    for (NSPasteboardItem *item in board.pasteboardItems) {
        NSPasteboardItem *copy = [NSPasteboardItem new];
        for (NSString *type in item.types) {
            NSData *data = [item dataForType:type];
            if (data) [copy setData:data forType:type];
        }
        [saved addObject:copy];
    }
    ProbeProvider *delegate = [ProbeProvider new];
    PlaceholderPresenter *presenter = nil;
    NSString *sourceDirectory = nil;
    PasteboardRef carbon = NULL;
    CarbonState state = {0};
    BOOL offered = NO;
    BOOL native = [mode isEqual:@"native-file"] || [mode isEqual:@"native-folder"] || [mode isEqual:@"native-fail"];
    nativeFail = [mode isEqual:@"native-fail"];
    if (native) {
        sourceDirectory = [NSTemporaryDirectory() stringByAppendingPathComponent:NSUUID.UUID.UUIDString];
        [NSFileManager.defaultManager createDirectoryAtPath:sourceDirectory withIntermediateDirectories:YES
            attributes:@{NSFilePosixPermissions: @0700} error:NULL];
        nativeRoot = [sourceDirectory stringByAppendingPathComponent:[mode isEqual:@"native-folder"] ? @"folder" : filename];
        if ([mode isEqual:@"native-folder"]) {
            [NSFileManager.defaultManager createDirectoryAtPath:[nativeRoot stringByAppendingPathComponent:@"nested"]
                withIntermediateDirectories:YES attributes:nil error:NULL];
            [NSFileManager.defaultManager createDirectoryAtPath:[nativeRoot stringByAppendingPathComponent:@"empty-dir"]
                withIntermediateDirectories:YES attributes:nil error:NULL];
            [NSFileManager.defaultManager createFileAtPath:[nativeRoot stringByAppendingPathComponent:@"nested/报告.txt"] contents:[NSData data] attributes:nil];
            [NSFileManager.defaultManager createFileAtPath:[nativeRoot stringByAppendingPathComponent:@"empty"] contents:[NSData data] attributes:nil];
        } else [NSFileManager.defaultManager createFileAtPath:nativeRoot contents:[NSData data] attributes:nil];
        int fd = open([[sourceDirectory stringByAppendingPathComponent:@".lock"] fileSystemRepresentation], O_CREAT|O_RDWR, 0600);
        if (fd >= 0 && flock(fd, LOCK_EX|LOCK_NB) == 0) {
            NSMutableArray *paths = [NSMutableArray arrayWithObject:nativeRoot];
            if ([mode isEqual:@"native-folder"]) {
                for (NSString *name in @[@"nested", @"nested/报告.txt", @"empty", @"empty-dir"])
                    [paths addObject:[nativeRoot stringByAppendingPathComponent:name]];
            }
            NSData *data = [NSJSONSerialization dataWithJSONObject:@{@"roots": @[nativeRoot], @"paths": paths} options:0 error:NULL];
            uint64_t revision = 0;
            offered = bk_clipboard_publish_files(data.bytes, data.length, 1, fd, (uint64_t)board.changeCount, &revision) == 1;
        }
        if (fd >= 0) close(fd);
        report([NSString stringWithFormat:@"NATIVE empty source: %@", nativeRoot]);
        report([NSString stringWithFormat:@"NATIVE source exists=%d presenters=%@", [NSFileManager.defaultManager fileExistsAtPath:nativeRoot], NSFileCoordinator.filePresenters]);
    } else if ([mode isEqual:@"appkit"]) {
        [board clearContents];
        NSFilePromiseProvider *provider = [[NSFilePromiseProvider alloc] initWithFileType:@"public.plain-text" delegate:delegate];
        offered = [board writeObjects:@[provider]];
    } else if ([mode isEqual:@"carbon"]) {
        OSStatus status = PasteboardCreate(kPasteboardClipboard, &carbon);
        if (!status) status = publishCarbon(carbon, &state);
        offered = status == noErr;
    } else {
        sourceDirectory = [NSTemporaryDirectory() stringByAppendingPathComponent:NSUUID.UUID.UUIDString];
        [NSFileManager.defaultManager createDirectoryAtPath:sourceDirectory withIntermediateDirectories:YES
            attributes:@{NSFilePosixPermissions: @0700} error:nil];
        delegate.sourceURL = [NSURL fileURLWithPath:[sourceDirectory stringByAppendingPathComponent:filename]];
        [board clearContents];
        if ([mode isEqual:@"presenter-file"] || [mode isEqual:@"presenter-empty"]) {
            BOOL created = [NSFileManager.defaultManager createFileAtPath:delegate.sourceURL.path
                contents:[NSData data] attributes:@{NSFilePosixPermissions: @0600}];
            if (created) {
                presenter = [PlaceholderPresenter new];
                presenter.leaveEmpty = [mode isEqual:@"presenter-empty"];
                presenter.presentedItemURL = delegate.sourceURL;
                [NSFileCoordinator addFilePresenter:presenter];
                offered = [board writeObjects:@[delegate.sourceURL]];
                report([NSString stringWithFormat:@"PRESENTER registered empty source: %@", delegate.sourceURL.path]);
            }
        } else if ([mode isEqual:@"local-file"]) {
            NSError *error = writeFile(delegate.sourceURL);
            offered = !error && [board writeObjects:@[delegate.sourceURL]];
            delegate.writes = !error;
        } else {
            NSPasteboardItem *item = [NSPasteboardItem new];
            [item setDataProvider:delegate forTypes:@[NSPasteboardTypeFileURL]];
            offered = [board writeObjects:@[item]];
        }
    }
    // Fulfilling a promise can change changeCount without changing its owner.
    NSString *markerType = @"com.anykvm.file-promise-probe";
    NSString *marker = NSUUID.UUID.UUIDString;
    if (carbon) {
        NSData *data = [marker dataUsingEncoding:NSUTF8StringEncoding];
        PasteboardPutItemFlavor(carbon, (PasteboardItemID)1,
            (__bridge CFStringRef)markerType, (__bridge CFDataRef)data, 0);
    } else {
        [board addTypes:@[markerType] owner:nil];
        [board setString:marker forType:markerType];
    }
    if (![[board stringForType:markerType] isEqual:marker]) {
        [board clearContents];
        if (saved.count) [board writeObjects:saved];
        if (carbon) { PasteboardSetPromiseKeeper(carbon, refusePromise, NULL); CFRelease(carbon); }
        if (presenter) [NSFileCoordinator removeFilePresenter:presenter];
        if (native) bk_clipboard_retire_files(1);
        if (sourceDirectory) [NSFileManager.defaultManager removeItemAtPath:sourceDirectory error:nil];
        report(@"Publication marker failed; original clipboard restored without testing.");
        return 1;
    }
    report([NSString stringWithFormat:@"PUBLISHED mode=%@ ok=%d duration=%.0fs types=%@", mode, offered, duration, board.types]);
    report([mode isEqual:@"local-file"] ? @"Local-file positive control: source file already exists." :
        @"Paste into an empty Finder folder now. No file content exists until a write callback.");
    pump(duration);
    report([NSString stringWithFormat:@"RESULT mode=%@ writes=%lu", mode,
        (unsigned long)(native ? nativeWrites : carbon ? state.calls : presenter ? presenter.writes : delegate.writes)]);
    if ([[board stringForType:markerType] isEqual:marker]) {
        [board clearContents];
        if (saved.count) [board writeObjects:saved];
        report(@"Original clipboard restored.");
    } else report(@"Newer clipboard retained.");
    if (carbon) { PasteboardSetPromiseKeeper(carbon, refusePromise, NULL); CFRelease(carbon); }
    if (native) bk_clipboard_retire_files(1);
    if (native) bk_clipboard_shutdown_files();
    if (presenter) {
        [NSFileCoordinator removeFilePresenter:presenter];
        [presenter.presentedItemOperationQueue waitUntilAllOperationsAreFinished];
    }
    if (sourceDirectory && !native) [NSFileManager.defaultManager removeItemAtPath:sourceDirectory error:nil];
    return offered ? 0 : 1;
}

int main(int argc, const char **argv) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        [NSApp finishLaunching];
        if (argc == 2 && !strcmp(argv[1], "--self-test")) return selfTest();
        if (argc == 2 && !strcmp(argv[1], "--inspect")) {
            report(NSPasteboard.generalPasteboard.types.description); return 0;
        }
        if (argc >= 3 && !strcmp(argv[1], "--publish") &&
                (!strcmp(argv[2], "appkit") || !strcmp(argv[2], "carbon") ||
                 !strcmp(argv[2], "file-url") || !strcmp(argv[2], "local-file") ||
                 !strcmp(argv[2], "presenter-file") || !strcmp(argv[2], "presenter-empty") ||
                 !strcmp(argv[2], "native-file") || !strcmp(argv[2], "native-folder") || !strcmp(argv[2], "native-fail"))) {
            double duration = argc == 4 ? strtod(argv[3], NULL) : 120;
            if (duration <= 0 || duration > 3600) return 2;
            return publish(@(argv[2]), duration);
        }
        fprintf(stderr, "Usage: %s --self-test | --inspect | --publish appkit|carbon|file-url|local-file|presenter-file|presenter-empty|native-file|native-folder|native-fail [seconds]\n", argv[0]);
        return 2;
    }
}
