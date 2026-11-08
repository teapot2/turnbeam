//go:build darwin && cgo

#import <Foundation/Foundation.h>
#import <LocalAuthentication/LocalAuthentication.h>
#import <Security/Security.h>
#include <stdlib.h>

void *turnbeam_auth_context(void) {
    LAContext *context = [[LAContext alloc] init];
    context.interactionNotAllowed = YES;
    return context;
}

void turnbeam_auth_cancel(void *context) { [(LAContext *)context invalidate]; }
void turnbeam_auth_release(void *context) { [(LAContext *)context release]; }

void turnbeam_secret_free(unsigned char *key, long length) {
    volatile unsigned char *p = key;
    for (long i = 0; i < length; ++i) p[i] = 0;
    free(key);
}

int turnbeam_keychain(void *context, int operation, const unsigned char *key, long key_len,
                  unsigned char **out, long *out_len) {
    @autoreleasepool {
        // Keep the service stable across renames; match the exact service/account.
        NSMutableDictionary *query = [NSMutableDictionary dictionaryWithDictionary:@{
            (id)kSecClass: (id)kSecClassGenericPassword,
            (id)kSecAttrService: @"org.turnbeam.meta-api",
            (id)kSecAttrAccount: @"META_API_KEY",
            (id)kSecAttrSynchronizable: @NO,
            (id)kSecUseAuthenticationContext: (LAContext *)context
        }];
        *out = NULL;
        *out_len = 0;
        if (operation == 0 || operation == 1) {
            query[(id)kSecMatchLimit] = (id)kSecMatchLimitOne;
            query[operation == 0 ? (id)kSecReturnData : (id)kSecReturnAttributes] = @YES;
            CFTypeRef result = NULL;
            OSStatus status = SecItemCopyMatching((CFDictionaryRef)query, &result);
            if (status == errSecSuccess && operation == 0) {
                if (!result || CFGetTypeID(result) != CFDataGetTypeID()) {
                    status = errSecDecode;
                } else {
                    CFDataRef data = (CFDataRef)result;
                    CFIndex length = CFDataGetLength(data);
                    if (length < 1 || length > 8192) {
                        status = errSecDecode;
                    } else {
                        *out = malloc((size_t)length);
                        if (!*out) status = errSecAllocate;
                        else {
                            memcpy(*out, CFDataGetBytePtr(data), (size_t)length);
                            *out_len = length;
                        }
                    }
                }
            }
            if (result) CFRelease(result);
            return status;
        }
        if (operation == 2) {
            NSData *data = [NSData dataWithBytes:key length:(NSUInteger)key_len];
            NSDictionary *changes = @{(id)kSecValueData: data};
            OSStatus status = SecItemUpdate((CFDictionaryRef)query, (CFDictionaryRef)changes);
            if (status == errSecItemNotFound) {
                query[(id)kSecValueData] = data;
                query[(id)kSecAttrLabel] = @"Turnbeam Meta API key";
                status = SecItemAdd((CFDictionaryRef)query, NULL);
            }
            return status;
        }
        if (operation == 3) return SecItemDelete((CFDictionaryRef)query);
        return errSecParam;
    }
}
