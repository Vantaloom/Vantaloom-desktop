//go:build darwin

// macOS 菜单栏托盘（Vantaloom/Vantaloom#117）。见 tray_darwin.go 的说明。
//
// 约束与 Windows 版相同：状态项（NSStatusItem）全进程只建一个、只在主线程上碰；
// 菜单打开时现取（menuNeedsUpdate），不缓存旧菜单；销毁时从状态栏移除。

#import <Cocoa/Cocoa.h>
#include <stdlib.h>

extern char *vtlTrayMenuJSON(void);
extern void vtlTrayOnSelect(int);

@interface VtlTrayController : NSObject <NSMenuDelegate>
@property(strong) NSStatusItem *item;
@property(strong) NSMenu *menu;
@end

@implementation VtlTrayController

- (void)menuNeedsUpdate:(NSMenu *)menu {
  [menu removeAllItems];
  char *raw = vtlTrayMenuJSON();
  if (raw == NULL) return;
  NSData *data = [NSData dataWithBytes:raw length:strlen(raw)];
  free(raw);
  NSArray *items = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
  if (![items isKindOfClass:[NSArray class]]) return;
  for (NSDictionary *it in items) {
    if ([it[@"separator"] boolValue]) {
      [menu addItem:[NSMenuItem separatorItem]];
      continue;
    }
    NSString *label = it[@"label"] ?: @"";
    NSMenuItem *mi = [[NSMenuItem alloc] initWithTitle:label action:nil keyEquivalent:@""];
    NSInteger ident = [it[@"id"] integerValue];
    mi.tag = ident;
    if (![it[@"disabled"] boolValue] && ident != 0) {
      mi.target = self;
      mi.action = @selector(onSelect:);
    } else {
      mi.enabled = NO;
    }
    [menu addItem:mi];
  }
}

- (void)onSelect:(NSMenuItem *)sender {
  vtlTrayOnSelect((int)sender.tag);
}

@end

static VtlTrayController *vtlController = nil;

void vtlTrayCreate(void) {
  dispatch_async(dispatch_get_main_queue(), ^{
    if (vtlController != nil) return; // 只建一个
    VtlTrayController *c = [VtlTrayController new];
    c.item = [[NSStatusBar systemStatusBar] statusItemWithLength:NSSquareStatusItemLength];
    NSImage *icon = [[NSApp applicationIconImage] copy];
    if (icon != nil) {
      icon.size = NSMakeSize(18, 18);
      c.item.button.image = icon;
    } else {
      c.item.button.title = @"V";
    }
    c.menu = [NSMenu new];
    c.menu.autoenablesItems = NO;
    c.menu.delegate = c;
    c.item.menu = c.menu;
    vtlController = c;
  });
}

void vtlTraySetTooltip(const char *text) {
  NSString *tip = [NSString stringWithUTF8String:text ?: ""];
  dispatch_async(dispatch_get_main_queue(), ^{
    if (vtlController == nil) return;
    vtlController.item.button.toolTip = tip;
  });
}

void vtlTrayDestroy(void) {
  dispatch_async(dispatch_get_main_queue(), ^{
    if (vtlController == nil) return;
    [[NSStatusBar systemStatusBar] removeStatusItem:vtlController.item];
    vtlController = nil;
  });
}
