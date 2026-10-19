#include "../screen_darwin.m"
#include <assert.h>
#include <string.h>

// Exercise native resume handling without opening a display or broadcasting
// a fake unlock notification to other applications.
int main(void) {
 @autoreleasepool {
  for (NSString *name in @[@"com.apple.screenIsUnlocked", NSWorkspaceDidWakeNotification, NSWorkspaceSessionDidBecomeActiveNotification]) {
   BKScreen *s = [[BKScreen alloc] init];
   s->lastStatus = SCFrameStatusIdle; s->completeFrames = 1;
   char detail[512];
   assert(bk_screen_idle_status(s, detail, sizeof(detail)) == 1);
   NSNotification *note = [NSNotification notificationWithName:name object:nil];
   if ([name isEqualToString:@"com.apple.screenIsUnlocked"]) [s desktopResumed:note];
   else [[[NSWorkspace sharedWorkspace] notificationCenter] postNotification:note];
   void *lease = NULL, *data = NULL, *pixel = NULL;
   int size, w, h, sw, sh; long long age;
   assert(bk_screen_take_geometry(s, &lease, &data, &pixel, &size, &w, &h, &sw, &sh, NULL, &age, detail, sizeof(detail)) == -1);
   assert(strstr(detail, "desktop resumed") != NULL);
   assert(bk_screen_idle_status(s, detail, sizeof(detail)) == 0);
   [s shutdown]; [s release];
  }
  BKScreen *closed = [[BKScreen alloc] init];
  [closed shutdown];
  [closed desktopResumed:[NSNotification notificationWithName:NSWorkspaceDidWakeNotification object:nil]];
  assert(closed->failure == nil);
  [closed release];
 }
 return 0;
}
