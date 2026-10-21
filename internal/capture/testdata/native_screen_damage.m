#include "../screen_darwin.m"
#include <assert.h>

static NSMutableDictionary *info(double scale, double contentScale, NSArray *rects) {
    return [@{SCStreamFrameInfoStatus: @(SCFrameStatusComplete),
        SCStreamFrameInfoScaleFactor: @(scale), SCStreamFrameInfoContentScale: @(contentScale),
        SCStreamFrameInfoContentRect: (NSDictionary *)[(id)CGRectCreateDictionaryRepresentation(CGRectMake(0, 0, 96, 64)) autorelease],
        SCStreamFrameInfoDirtyRects: rects} mutableCopy];
}
static CMSampleBufferRef sample(CVPixelBufferRef pixel, NSDictionary *info) {
    CMVideoFormatDescriptionRef format = NULL;
    assert(CMVideoFormatDescriptionCreateForImageBuffer(NULL, pixel, &format) == noErr);
    CMSampleTimingInfo timing = {kCMTimeInvalid, kCMTimeZero, kCMTimeInvalid};
    CMSampleBufferRef sample = NULL;
    assert(CMSampleBufferCreateReadyWithImageBuffer(NULL, pixel, format, &timing, &sample) == noErr);
    NSMutableDictionary *attachment = ((NSArray *)CMSampleBufferGetSampleAttachmentsArray(sample, true))[0];
    [attachment addEntriesFromDictionary:info];
    CFRelease(format);
    return sample;
}
static void deliver(BKScreen *s, CVPixelBufferRef pixel, NSDictionary *info) {
    CMSampleBufferRef frame = sample(pixel, info);
    [s stream:nil didOutputSampleBuffer:frame ofType:SCStreamOutputTypeScreen];
    CFRelease(frame);
}
static bk_screen_damage take(BKScreen *s, void **lease) {
    void *data, *pixel; int size,w,h,sw,sh; long long age; char error[512];
    assert(bk_screen_take_geometry(s, lease, &data, &pixel, &size, &w, &h, &sw, &sh, NULL, &age, error, sizeof(error)) == 1);
    bk_screen_damage d = {0};
    assert(bk_screen_lease_damage(*lease, &d));
    return d;
}
int main(void) {
 @autoreleasepool {
    BKScreen *s = [BKScreen new]; s->width = 192; s->height = 128;
    s->sourceWidth = 384; s->sourceHeight = 256;
    bk_screen_set_damage(s, 1);
    CVPixelBufferRef pixel = NULL;
    assert(CVPixelBufferCreate(NULL, 192, 128, kCVPixelFormatType_32BGRA, NULL, &pixel) == noErr);
    // Retina metadata: dirty pixels must not be multiplied by scale=2.
    NSArray *rects = @[[NSValue valueWithRect:CGRectMake(63.5, 1.2, 1.1, 2.2)],
                      [NSValue valueWithRect:CGRectMake(64, 1, 2, 3)]];
    NSMutableDictionary *metadata = info(2, 0.5, rects);
    void *lease = NULL;
    deliver(s,pixel,metadata); bk_screen_damage d = take(s,&lease);
    assert(!d.valid && d.sequence == 1 && d.previous == 0); bk_screen_release(lease);
    deliver(s,pixel,metadata); d = take(s,&lease);
    assert(d.valid && d.sequence == 2 && d.count == 2);
    assert(d.rects[0].x0 == 63 && d.rects[0].x1 == 65 && d.rects[0].y1 == 4);
    // Metadata remains owned by the lease after queue replacement.
    metadata[SCStreamFrameInfoDirtyRects] = @[[NSValue valueWithRect:CGRectMake(130,70,1,1)]];
    deliver(s,pixel,metadata);
    metadata[SCStreamFrameInfoDirtyRects] = rects;
    deliver(s,pixel,metadata);
    assert(d.rects[0].x0 == 63); bk_screen_release(lease);
    d = take(s,&lease); assert(d.valid && d.sequence == 4 && d.previous == 2 && d.count == 2);
    assert(d.rects[0].x0 == 0 && d.rects[0].x1 == 128);
    assert(d.rects[1].x0 == 128 && d.rects[1].y0 == 64); bk_screen_release(lease);
    metadata[SCStreamFrameInfoDirtyRects] = @[];
    deliver(s,pixel,metadata); d = take(s,&lease);
    assert(d.valid && d.count == 0); bk_screen_release(lease);
    [metadata removeObjectForKey:SCStreamFrameInfoDirtyRects];
    deliver(s,pixel,metadata); d = take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    metadata[SCStreamFrameInfoDirtyRects] = rects;
    metadata[SCStreamFrameInfoContentScale] = @0.75;
    deliver(s,pixel,metadata); d = take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    deliver(s,pixel,metadata); d = take(s,&lease); assert(d.valid); bk_screen_release(lease);
    metadata[SCStreamFrameInfoScaleFactor] = @1;
    deliver(s,pixel,metadata); d = take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    metadata[SCStreamFrameInfoContentRect] = (NSDictionary *)[(id)CGRectCreateDictionaryRepresentation(CGRectMake(1,0,95,64)) autorelease];
    deliver(s,pixel,metadata); d = take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    for (id bad in @[@[[NSValue valueWithRect:CGRectMake(-1,0,2,2)]],
                       @[[NSValue valueWithRect:CGRectMake(190,0,4,2)]],
                       @[@"bad"], @42]) {
      metadata[SCStreamFrameInfoDirtyRects] = bad;
      deliver(s,pixel,metadata); d = take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    }
    [metadata removeObjectForKey:SCStreamFrameInfoContentScale];
    metadata[SCStreamFrameInfoDirtyRects] = @[];
    deliver(s,pixel,metadata); d = take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    metadata[SCStreamFrameInfoContentScale] = @0.75;
    deliver(s,pixel,metadata); take(s,&lease); bk_screen_release(lease);
    metadata[SCStreamFrameInfoDirtyRects] = @[[NSValue valueWithRect:CGRectMake(1,1,1,1)]];
    deliver(s,pixel,metadata);
    [metadata removeObjectForKey:SCStreamFrameInfoDirtyRects];
    deliver(s,pixel,metadata);
    metadata[SCStreamFrameInfoDirtyRects] = @[];
    deliver(s,pixel,metadata);
    d=take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    // Full coverage falls back and stays poisoned through further replacement.
    metadata[SCStreamFrameInfoDirtyRects] = @[[NSValue valueWithRect:CGRectMake(0,0,192,128)]];
    deliver(s,pixel,metadata);
    metadata[SCStreamFrameInfoDirtyRects] = @[];
    deliver(s,pixel,metadata);
    d=take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    deliver(s,pixel,metadata); d=take(s,&lease); assert(d.valid); bk_screen_release(lease);
    // A geometry transition inside a replaced interval cannot be hidden.
    deliver(s,pixel,metadata);
    metadata[SCStreamFrameInfoContentScale] = @0.5;
    deliver(s,pixel,metadata); deliver(s,pixel,metadata);
    d=take(s,&lease); assert(!d.valid); bk_screen_release(lease);
    uint64_t seq = d.sequence;
    metadata[SCStreamFrameInfoStatus] = @(SCFrameStatusIdle);
    deliver(s,pixel,metadata); assert(s->damageSequence == seq);
    [metadata removeObjectForKey:SCStreamFrameInfoStatus];
    deliver(s,pixel,metadata); assert(s->damageSequence == seq+1);
    [s stream:nil didOutputSampleBuffer:NULL ofType:SCStreamOutputTypeScreen];
    assert(s->damageSequence == seq+2);
    metadata[SCStreamFrameInfoStatus] = @(SCFrameStatusBlank);
    deliver(s,pixel,metadata); assert(s->damageSequence == seq+3);
    BKScreen *other = [BKScreen new]; assert(other->damageStream != s->damageStream);
    [other shutdown]; [other release];
    [metadata release]; CFRelease(pixel); [s shutdown]; [s release];
 }
 return 0;
}
