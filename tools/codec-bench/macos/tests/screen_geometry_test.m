// Synthetic ScreenCaptureKit frames exercise production metadata handling.
// No real display capture or input injection is performed.
#import "../../../../internal/capture/screen_darwin.m"
#include <assert.h>

static NSDictionary *info(CGRect rect, double scale) {
 NSDictionary *dict = [(NSDictionary *)CGRectCreateDictionaryRepresentation(rect) autorelease];
 return @{SCStreamFrameInfoContentRect:dict, SCStreamFrameInfoScaleFactor:@(scale),
   SCStreamFrameInfoContentScale:@0.5, SCStreamFrameInfoStatus:@(SCFrameStatusComplete)};
}

static void frame(BKScreen *s, CVPixelBufferRef pixel, CGRect expected) {
 CMVideoFormatDescriptionRef format = NULL;
 assert(!CMVideoFormatDescriptionCreateForImageBuffer(NULL, pixel, &format));
 CMSampleTimingInfo timing = {CMTimeMake(1,30),CMClockGetTime(CMClockGetHostTimeClock()),kCMTimeInvalid};
 CMSampleBufferRef sample = NULL;
 assert(!CMSampleBufferCreateReadyWithImageBuffer(NULL,pixel,format,&timing,&sample));
 CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sample,true);
 CFMutableDictionaryRef attachment = (CFMutableDictionaryRef)CFArrayGetValueAtIndex(attachments,0);
 CGRect points = CGRectApplyAffineTransform(expected,CGAffineTransformMakeScale(0.5,0.5));
 NSDictionary *metadata = info(points,2);
 for (NSString *key in metadata) CFDictionarySetValue(attachment,(CFStringRef)key,metadata[key]);
 dispatch_sync(s->queue,^{ [s stream:(SCStream *)s didOutputSampleBuffer:sample ofType:SCStreamOutputTypeScreen]; });
 int size,w,h,sw,sh,rect[4]; long long age; void *data=NULL; char error[1024];
 assert(bk_screen_read_geometry(s,&data,&size,&w,&h,&sw,&sh,rect,&age,error,sizeof(error))==1);
 assert(w==1280 && h==720 && sw==2880 && sh==1800);
 assert(rect[0]==expected.origin.x && rect[1]==expected.origin.y);
 assert(rect[2]==expected.size.width && rect[3]==expected.size.height);
 assert(size==(s->codec==3 ? 1280*720*3/2 : 1280*720*4));
 free(data); CFRelease(sample); CFRelease(format);
}

int main(void) { @autoreleasepool {
 for (int codec=0;codec<=3;codec+=3) {
  CVPixelBufferRef pixel=NULL;
  assert(!CVPixelBufferCreate(NULL,1280,720,codec ? kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange : kCVPixelFormatType_32BGRA,NULL,&pixel));
  CGRect expected=CGRectMake(32,18,1216,684);
  assert(CGRectEqualToRect(bk_frame_content_rect(info(CGRectMake(16,9,608,342),2),pixel),expected));
  assert(CGRectEqualToRect(bk_frame_content_rect(info(CGRectMake(0,0,640,360),2),pixel),CGRectMake(0,0,1280,720)));
  assert(CGRectIsEmpty(bk_frame_content_rect(@{},pixel)));
  assert(CGRectIsEmpty(bk_frame_content_rect(info(CGRectMake(0,0,640,360),NAN),pixel)));
  assert(CGRectIsEmpty(bk_frame_content_rect(info(CGRectMake(-1,0,640,360),2),pixel)));
  assert(CGRectIsEmpty(bk_frame_content_rect(info(CGRectMake(0,0,1280,720),2),pixel)));
  BKScreen *s=[BKScreen new]; s->codec=codec; s->fps=30; s->width=1280; s->height=720; s->sourceWidth=2880; s->sourceHeight=1800;
  frame(s,pixel,expected);
  frame(s,pixel,CGRectMake(280,0,720,720));
  [s shutdown]; [s release]; CFRelease(pixel);
 }
 puts("screen frame geometry: BGRA/NV12 metadata, Retina scale, shifted content and invalid metadata passed");
 return 0;
}}
