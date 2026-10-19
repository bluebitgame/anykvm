#import <AppKit/AppKit.h>
#include "clipboard_darwin.h"
#include "mainloop_darwin.h"
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int bk_clipboard_read(char **text, size_t *length, uint64_t *revision) {
    __block int status = 0;
    *text = NULL;
    *length = 0;
    void (^read)(void) = ^{
        @autoreleasepool {
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            if (!board) { status = -1; return; }
            *revision = (uint64_t)board.changeCount;
            NSString *value = [board stringForType:NSPasteboardTypeString];
            if (!value) return;
            NSData *data = [value dataUsingEncoding:NSUTF8StringEncoding];
            if (!data || data.length > (2 << 20)) return;
            if (*revision != (uint64_t)board.changeCount) { status = -1; return; }
            char *copy = malloc(data.length + 1);
            if (!copy) { status = -1; return; }
            if (data.length) memcpy(copy, data.bytes, data.length);
            copy[data.length] = 0;
            *text = copy;
            *length = data.length;
            status = 1;
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), read);
    else read();
    return status;
}

int bk_clipboard_write(const char *text, size_t length, uint64_t *revision) {
    __block int status = 0;
    void (^write)(void) = ^{
        @autoreleasepool {
            NSString *value = [[NSString alloc] initWithBytes:text length:length encoding:NSUTF8StringEncoding];
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            if (value && board) {
                [board clearContents];
                status = [board setString:value forType:NSPasteboardTypeString] ? 1 : 0;
                *revision = (uint64_t)board.changeCount;
            }
            [value release];
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), write);
    else write();
    return status;
}

static int bk_clipboard_copy_data(NSData *data, char **output, size_t *length) {
    if (!data) return 0;
    if (data.length > (16 << 20)) return -1;
    char *copy = malloc(data.length ? data.length : 1);
    if (!copy) return -1;
    if (data.length) memcpy(copy, data.bytes, data.length);
    *output = copy;
    *length = data.length;
    return 1;
}

int bk_clipboard_read_rich(char **html, size_t *html_length, char **rtf, size_t *rtf_length, uint64_t *revision) {
    __block int status = 0;
    *html = NULL; *html_length = 0; *rtf = NULL; *rtf_length = 0;
    void (^read)(void) = ^{
        @autoreleasepool {
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            if (!board) { status = -1; return; }
            *revision = (uint64_t)board.changeCount;
            int html_status = bk_clipboard_copy_data([board dataForType:NSPasteboardTypeHTML], html, html_length);
            int rtf_status = bk_clipboard_copy_data([board dataForType:NSPasteboardTypeRTF], rtf, rtf_length);
            if (html_status < 0 || rtf_status < 0 || *revision != (uint64_t)board.changeCount) {
                free(*html); free(*rtf); *html = NULL; *rtf = NULL; *html_length = 0; *rtf_length = 0; status = -1; return;
            }
            status = html_status || rtf_status;
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), read); else read();
    return status;
}

int bk_clipboard_write_rich(const char *html, size_t html_length, const char *rtf, size_t rtf_length, uint64_t *revision) {
    __block int status = 0;
    if ((!html && html_length) || (!rtf && rtf_length) || html_length > (16 << 20) || rtf_length > (16 << 20)) return 0;
    void (^write)(void) = ^{
        @autoreleasepool {
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            NSData *html_data = html ? [NSData dataWithBytes:html length:html_length] : nil;
            NSData *rtf_data = rtf ? [NSData dataWithBytes:rtf length:rtf_length] : nil;
            if (!board || (!html_data && !rtf_data)) return;
            [board clearContents];
            BOOL ok = YES;
            if (html_data) ok = ok && [board setData:html_data forType:NSPasteboardTypeHTML];
            if (rtf_data) ok = ok && [board setData:rtf_data forType:NSPasteboardTypeRTF];
            NSAttributedString *attributed = nil;
            if (rtf_data) attributed = [[NSAttributedString alloc] initWithRTF:rtf_data documentAttributes:NULL];
            else if (html_data) attributed = [[NSAttributedString alloc] initWithHTML:html_data documentAttributes:NULL];
            if (attributed) ok = ok && [board setString:attributed.string forType:NSPasteboardTypeString];
            [attributed release];
            status = ok ? 1 : 0;
            *revision = (uint64_t)board.changeCount;
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), write); else write();
    return status;
}

int bk_clipboard_read_image(char **png, size_t *length, uint64_t *revision) {
    __block int status = 0;
    *png = NULL; *length = 0;
    void (^read)(void) = ^{
        @autoreleasepool {
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            if (!board) { status = -1; return; }
            *revision = (uint64_t)board.changeCount;
            NSData *data = [board dataForType:NSPasteboardTypePNG];
            if (!data) {
                NSImage *image = [[NSImage alloc] initWithPasteboard:board];
                NSBitmapImageRep *representation = image ? [[NSBitmapImageRep alloc] initWithData:image.TIFFRepresentation] : nil;
                data = [representation representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
                [representation release]; [image release];
            }
            status = bk_clipboard_copy_data(data, png, length);
            if (status < 0 || *revision != (uint64_t)board.changeCount) { free(*png); *png = NULL; *length = 0; status = -1; }
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), read); else read();
    return status;
}

int bk_clipboard_write_image(const char *png, size_t length, uint64_t *revision) {
    __block int status = 0;
    if (!png || !length || length > (16 << 20)) return 0;
    void (^write)(void) = ^{
        @autoreleasepool {
            NSData *data = [NSData dataWithBytes:png length:length];
            NSImage *image = [[NSImage alloc] initWithData:data];
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            if (image && board) {
                [board clearContents];
                BOOL ok = [board setData:data forType:NSPasteboardTypePNG];
                NSData *tiff = image.TIFFRepresentation;
                if (tiff) ok = ok && [board setData:tiff forType:NSPasteboardTypeTIFF];
                status = ok ? 1 : 0;
                *revision = (uint64_t)board.changeCount;
            }
            [image release];
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), write); else write();
    return status;
}

int bk_clipboard_read_files(char **json, size_t *length, uint64_t *revision) {
    __block int status = 0;
    *json = NULL;
    *length = 0;
    void (^read)(void) = ^{
        @autoreleasepool {
            NSPasteboard *board = [NSPasteboard generalPasteboard];
            if (!board) { status = -1; return; }
            *revision = (uint64_t)board.changeCount;
            NSString *marker = [board stringForType:@"com.anykvm.clipboard.offer"];
            NSString *prefix = [NSString stringWithFormat:@"%d-", getpid()];
            if ([marker hasPrefix:prefix] && !bk_clipboard_file_offer_ready([[marker substringFromIndex:prefix.length] longLongValue])) {
                status = 2; return;
            }
            NSArray *urls = [board readObjectsForClasses:@[[NSURL class]]
                options:@{NSPasteboardURLReadingFileURLsOnlyKey: @YES}];
            if (!urls.count) return;
            if (urls.count > 1024) { status = -1; return; }
            NSMutableArray *paths = [NSMutableArray array];
            for (NSURL *url in urls) {
                if (!url.isFileURL || !url.path) { status = -1; return; }
                [paths addObject:url.path];
            }
            NSData *data = [NSJSONSerialization dataWithJSONObject:paths options:0 error:NULL];
            if (!data || data.length > (2 << 20) || *revision != (uint64_t)board.changeCount) {
                status = -1; return;
            }
            char *copy = malloc(data.length + 1);
            if (!copy) { status = -1; return; }
            memcpy(copy, data.bytes, data.length);
            copy[data.length] = 0;
            *json = copy;
            *length = data.length;
            status = 1;
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), read);
    else read();
    return status;
}
