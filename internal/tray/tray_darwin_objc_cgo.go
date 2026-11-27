//go:build darwin && cgo

package tray

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa

#include <stdint.h>
#include <stdlib.h>
#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>

extern void flitnixDarwinTrayAction(uintptr_t handle, char *actionID);

@interface FlitnixTrayTarget : NSObject
@property (nonatomic, assign) uintptr_t handle;
@end

@implementation FlitnixTrayTarget
- (void)menuItemClicked:(id)sender {
	NSMenuItem *item = (NSMenuItem *)sender;
	NSString *act = [item representedObject];
	if (act != nil) {
		flitnixDarwinTrayAction(self.handle, (char *)[act UTF8String]);
	}
}
@end

typedef struct {
	NSStatusItem *statusItem;
	FlitnixTrayTarget *target;
	NSData *iconData;
} FlitnixDarwinTray;

static NSMenuItem *buildDarwinMenuItem(const char *action, const char *label, int enabled, int checked, FlitnixTrayTarget *target) {
	NSString *title = label ? [NSString stringWithUTF8String:label] : @"";
	SEL sel = enabled ? @selector(menuItemClicked:) : nil;
	NSMenuItem *mi = [[NSMenuItem alloc] initWithTitle:title action:sel keyEquivalent:@""];
	if (enabled) {
		[mi setTarget:target];
		if (action) {
			[mi setRepresentedObject:[NSString stringWithUTF8String:action]];
		}
	}
	[mi setEnabled:(enabled != 0)];
	[mi setState:(checked ? NSControlStateValueOn : NSControlStateValueOff)];
	return mi;
}

static void *flitnixTrayCreate(uintptr_t handle, const void *pngBytes, int pngLen) {
	FlitnixDarwinTray *ctx = (FlitnixDarwinTray *)calloc(1, sizeof(FlitnixDarwinTray));
	ctx->target = [[FlitnixTrayTarget alloc] init];
	ctx->target.handle = handle;
	if (pngBytes != NULL && pngLen > 0) {
		ctx->iconData = [NSData dataWithBytes:pngBytes length:(NSUInteger)pngLen];
	}
	dispatch_sync(dispatch_get_main_queue(), ^{
		NSStatusBar *bar = [NSStatusBar systemStatusBar];
		ctx->statusItem = [bar statusItemWithLength:NSSquareStatusItemLength];
		NSStatusBarButton *btn = ctx->statusItem.button;
		if (btn != nil) {
			if (ctx->iconData != nil) {
				NSImage *img = [[NSImage alloc] initWithData:ctx->iconData];
				[img setSize:NSMakeSize(18, 18)];
				[btn setImage:img];
			} else {
				[btn setTitle:@"F"];
			}
		}
	});
	return (void *)ctx;
}

static void flitnixTraySetTooltip(void *ptr, const char *tooltip) {
	FlitnixDarwinTray *ctx = (FlitnixDarwinTray *)ptr;
	if (ctx == NULL || ctx->statusItem == nil) return;
	NSString *tip = tooltip ? [NSString stringWithUTF8String:tooltip] : @"Flitnix";
	dispatch_async(dispatch_get_main_queue(), ^{
		if (ctx->statusItem.button != nil) {
			[ctx->statusItem.button setToolTip:tip];
		}
	});
}

static void flitnixTrayBeginMenuUpdate(void *ptr) {
	FlitnixDarwinTray *ctx = (FlitnixDarwinTray *)ptr;
	if (ctx == NULL) return;
	dispatch_async(dispatch_get_main_queue(), ^{
		NSMenu *menu = [[NSMenu alloc] initWithTitle:@"Flitnix"];
		[menu setAutoenablesItems:NO];
		ctx->statusItem.menu = menu;
	});
}

static void flitnixTrayAddMenuItem(void *ptr, const char *action, const char *label, int isSep, int enabled, int checked) {
	FlitnixDarwinTray *ctx = (FlitnixDarwinTray *)ptr;
	if (ctx == NULL) return;
	NSString *aStr = action ? [NSString stringWithUTF8String:action] : nil;
	NSString *lStr = label ? [NSString stringWithUTF8String:label] : @"";
	dispatch_async(dispatch_get_main_queue(), ^{
		NSMenu *menu = ctx->statusItem.menu;
		if (menu == nil) return;
		if (isSep) {
			[menu addItem:[NSMenuItem separatorItem]];
			return;
		}
		NSMenuItem *mi = buildDarwinMenuItem(aStr ? [aStr UTF8String] : NULL, [lStr UTF8String], enabled, checked, ctx->target);
		[menu addItem:mi];
	});
}

static void flitnixTrayAddSubmenuItem(void *ptr, const char *parentLabel, int parentEnabled) {
	FlitnixDarwinTray *ctx = (FlitnixDarwinTray *)ptr;
	if (ctx == NULL) return;
	NSString *lStr = parentLabel ? [NSString stringWithUTF8String:parentLabel] : @"";
	dispatch_async(dispatch_get_main_queue(), ^{
		NSMenu *menu = ctx->statusItem.menu;
		if (menu == nil) return;
		NSMenuItem *mi = [[NSMenuItem alloc] initWithTitle:lStr action:nil keyEquivalent:@""];
		[mi setEnabled:(parentEnabled != 0)];
		NSMenu *sub = [[NSMenu alloc] initWithTitle:lStr];
		[sub setAutoenablesItems:NO];
		[mi setSubmenu:sub];
		[menu addItem:mi];
	});
}

static void flitnixTrayAddSubmenuChild(void *ptr, const char *action, const char *label, int enabled, int checked) {
	FlitnixDarwinTray *ctx = (FlitnixDarwinTray *)ptr;
	if (ctx == NULL) return;
	NSString *aStr = action ? [NSString stringWithUTF8String:action] : nil;
	NSString *lStr = label ? [NSString stringWithUTF8String:label] : @"";
	dispatch_async(dispatch_get_main_queue(), ^{
		NSMenu *menu = ctx->statusItem.menu;
		if (menu == nil || [menu numberOfItems] == 0) return;
		NSMenuItem *last = [menu itemAtIndex:[menu numberOfItems] - 1];
		NSMenu *sub = [last submenu];
		if (sub == nil) return;
		NSMenuItem *child = buildDarwinMenuItem(aStr ? [aStr UTF8String] : NULL, [lStr UTF8String], enabled, checked, ctx->target);
		[sub addItem:child];
	});
}

static void flitnixTrayDestroy(void *ptr) {
	FlitnixDarwinTray *ctx = (FlitnixDarwinTray *)ptr;
	if (ctx == NULL) return;
	dispatch_sync(dispatch_get_main_queue(), ^{
		if (ctx->statusItem != nil) {
			[[NSStatusBar systemStatusBar] removeStatusItem:ctx->statusItem];
			ctx->statusItem = nil;
		}
		ctx->target = nil;
		ctx->iconData = nil;
	});
	free(ctx);
}
*/
import "C"
import (
	"runtime/cgo"
	"unsafe"
)

func (b *darwinBackend) Start(initial State, initialMenu []MenuItem, onAction func(id string)) bool {
	b.mu.Lock()
	b.onAction = onAction
	b.handle = cgo.NewHandle(b)
	var pngPtr unsafe.Pointer
	var pngLen C.int
	if len(b.iconPNG) > 0 {
		pngPtr = unsafe.Pointer(&b.iconPNG[0])
		pngLen = C.int(len(b.iconPNG))
	}
	ptr := C.flitnixTrayCreate(C.uintptr_t(b.handle), pngPtr, pngLen)
	b.ptr = ptr
	b.mu.Unlock()

	if ptr == nil {
		return false
	}
	b.Update(initial, initialMenu)
	return true
}

func (b *darwinBackend) Update(state State, menu []MenuItem) {
	b.mu.Lock()
	ptr := b.ptr
	b.mu.Unlock()
	if ptr == nil {
		return
	}
	cTip := C.CString(state.Tooltip)
	C.flitnixTraySetTooltip(ptr, cTip)
	C.free(unsafe.Pointer(cTip))

	C.flitnixTrayBeginMenuUpdate(ptr)
	for _, item := range menu {
		if !item.Visible {
			continue
		}
		if item.IsSeparator {
			C.flitnixTrayAddMenuItem(ptr, nil, nil, 1, 0, 0)
			continue
		}
		if len(item.Children) > 0 {
			cLbl := C.CString(item.Label)
			C.flitnixTrayAddSubmenuItem(ptr, cLbl, boolInt(item.Enabled))
			C.free(unsafe.Pointer(cLbl))
			for _, child := range item.Children {
				if !child.Visible {
					continue
				}
				cAct := C.CString(child.ID)
				cSubLbl := C.CString(child.Label)
				C.flitnixTrayAddSubmenuChild(ptr, cAct, cSubLbl, boolInt(child.Enabled), boolInt(child.Checked))
				C.free(unsafe.Pointer(cAct))
				C.free(unsafe.Pointer(cSubLbl))
			}
			continue
		}
		cAct := C.CString(item.ID)
		cLbl := C.CString(item.Label)
		C.flitnixTrayAddMenuItem(ptr, cAct, cLbl, 0, boolInt(item.Enabled), boolInt(item.Checked))
		C.free(unsafe.Pointer(cAct))
		C.free(unsafe.Pointer(cLbl))
	}
}

func (b *darwinBackend) Stop() {
	b.mu.Lock()
	ptr := b.ptr
	h := b.handle
	b.ptr = nil
	b.handle = 0
	b.mu.Unlock()
	if ptr != nil {
		C.flitnixTrayDestroy(ptr)
	}
	if h != 0 {
		h.Delete()
	}
}

func boolInt(v bool) C.int {
	if v {
		return 1
	}
	return 0
}
