//go:build darwin

#import <CoreWLAN/CoreWLAN.h>
#import <Foundation/Foundation.h>
#include <stddef.h>
#include <string.h>

int flitnix_corewlan_metadata(const char *interface_name, char *ssid, size_t ssid_size) {
    if (interface_name == NULL || ssid == NULL || ssid_size == 0) {
        return 0;
    }
    ssid[0] = '\0';

    @autoreleasepool {
        NSString *name = [NSString stringWithUTF8String:interface_name];
        if (name == nil) {
            return 0;
        }
        CWWiFiClient *client = [CWWiFiClient sharedWiFiClient];
        CWInterface *interface = [client interfaceWithName:name];
        if (interface == nil) {
            return 0;
        }
        NSString *networkName = [interface ssid];
        if (networkName == nil || networkName.length == 0) {
            return 0;
        }
        NSData *encoded = [networkName dataUsingEncoding:NSUTF8StringEncoding];
        if (encoded == nil || encoded.length == 0) {
            return 0;
        }
        size_t copyLength = encoded.length;
        if (copyLength >= ssid_size) {
            copyLength = ssid_size - 1;
        }
        memcpy(ssid, encoded.bytes, copyLength);
        ssid[copyLength] = '\0';
    }
    return ssid[0] == '\0' ? 0 : 1;
}
