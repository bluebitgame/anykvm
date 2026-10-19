#import <Foundation/Foundation.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdatomic.h>
#include "mainloop_darwin.h"

static _Atomic int active;
static CFRunLoopSourceRef keep_alive;
static void source_perform(void *info) { (void)info; }

int bk_mainloop_prepare(void) {
    if (![NSThread isMainThread] || atomic_load(&active)) return 0;
    CFRunLoopSourceContext context = {0};
    context.perform = source_perform;
    keep_alive = CFRunLoopSourceCreate(NULL, 0, &context);
    if (!keep_alive) return 0;
    CFRunLoopAddSource(CFRunLoopGetMain(), keep_alive, kCFRunLoopDefaultMode);
    atomic_store(&active, 1);
    return 1;
}

void bk_mainloop_run(void) {
    while (atomic_load(&active)) {
        @autoreleasepool {
            CFRunLoopRunInMode(kCFRunLoopDefaultMode, 60, true);
        }
    }
    CFRunLoopRemoveSource(CFRunLoopGetMain(), keep_alive, kCFRunLoopDefaultMode);
    CFRelease(keep_alive);
    keep_alive = NULL;
}

void bk_mainloop_stop(void) {
    CFRunLoopRef loop = CFRunLoopGetMain();
    // Queueing the stop also handles a fast CLI command finishing before run.
    CFRunLoopPerformBlock(loop, kCFRunLoopDefaultMode, ^{
        atomic_store(&active, 0);
        CFRunLoopStop(loop);
    });
    CFRunLoopWakeUp(loop);
}

int bk_mainloop_active(void) { return atomic_load(&active); }
