//go:build darwin && cgo

#import <Foundation/Foundation.h>
#include "capacity_darwin.h"

int boxpool_important_capacity(const char *path, int64_t *available) {
    @autoreleasepool {
        NSURL *url = [NSURL fileURLWithFileSystemRepresentation:path
                                                  isDirectory:YES
                                                relativeToURL:nil];
        // Match statfs when a state/backend directory points at another volume.
        url = [url URLByResolvingSymlinksInPath];
        id value = nil;
        if (![url getResourceValue:&value
                           forKey:NSURLVolumeAvailableCapacityForImportantUsageKey
                            error:NULL] || ![value isKindOfClass:[NSNumber class]]) {
            return 0;
        }
        int64_t bytes = [value longLongValue];
        if (bytes < 0) return 0;
        *available = bytes;
        return 1;
    }
}
