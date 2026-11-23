// Real, bounded native UI diagnostics. No authentication or text entry occurs.
// Electron documents AXManualAccessibility in docs/tutorial/accessibility.md.
#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#import <libproc.h>
#import <sys/proc_info.h>
#import <time.h>
#import <unistd.h>
#import <limits.h>
#import <errno.h>
#import <math.h>
#import <stdint.h>
#import <stdio.h>
#import <stdlib.h>

static const NSUInteger MaxInspectionElements = 512;
static const NSUInteger MaxInspectionDepth = 24;
static const NSUInteger MaxFocusAttempts = 32;

static double monotonicNow(void) {
    struct timespec value;
    if (clock_gettime(CLOCK_MONOTONIC, &value) != 0) return 0;
    return (double)value.tv_sec + (double)value.tv_nsec / 1000000000.0;
}

@interface UIContext : NSObject
@property(nonatomic) pid_t pid;
@property(nonatomic) uint64_t startSec;
@property(nonatomic) uint64_t startUsec;
@property(nonatomic, copy) NSString *executable;
@property(nonatomic, copy) NSString *bundle;
@property(nonatomic, copy) NSString *bundleID;
@property(nonatomic) double normalDeadline;
@property(nonatomic) double hardDeadline;
@property(nonatomic) BOOL identityFailed;
@property(nonatomic) BOOL budgetExceeded;
@property(nonatomic, strong) NSMutableArray<NSString *> *barriers;
@end
@implementation UIContext
@end

static NSString *axName(AXError error) {
    switch (error) {
        case kAXErrorSuccess: return @"success";
        case kAXErrorFailure: return @"failure";
        case kAXErrorIllegalArgument: return @"illegal_argument";
        case kAXErrorInvalidUIElement: return @"invalid_ui_element";
        case kAXErrorInvalidUIElementObserver: return @"invalid_ui_element_observer";
        case kAXErrorCannotComplete: return @"cannot_complete";
        case kAXErrorAttributeUnsupported: return @"attribute_unsupported";
        case kAXErrorActionUnsupported: return @"action_unsupported";
        case kAXErrorNotificationUnsupported: return @"notification_unsupported";
        case kAXErrorNotImplemented: return @"not_implemented";
        case kAXErrorNotificationAlreadyRegistered: return @"notification_already_registered";
        case kAXErrorNotificationNotRegistered: return @"notification_not_registered";
        case kAXErrorAPIDisabled: return @"api_disabled";
        case kAXErrorNoValue: return @"no_value";
        case kAXErrorParameterizedAttributeUnsupported: return @"parameterized_attribute_unsupported";
        case kAXErrorNotEnoughPrecision: return @"not_enough_precision";
        default: return @"unknown_ax_error";
    }
}

static void barrier(UIContext *context, NSString *code) {
    if (![context.barriers containsObject:code]) [context.barriers addObject:code];
}

static BOOL exactIdentity(UIContext *context) {
    struct proc_bsdinfo info = {0};
    int size = proc_pidinfo(context.pid, PROC_PIDTBSDINFO, 0, &info, (int)sizeof(info));
    char path[PROC_PIDPATHINFO_MAXSIZE] = {0};
    BOOL valid = size == (int)sizeof(info) && info.pbi_pid == (uint32_t)context.pid &&
        info.pbi_uid == getuid() && info.pbi_start_tvsec == context.startSec &&
        info.pbi_start_tvusec == context.startUsec &&
        proc_pidpath(context.pid, path, (uint32_t)sizeof(path)) > 0;
    NSString *actualPath = valid ? [NSString stringWithUTF8String:path] : nil;
    NSRunningApplication *application = valid ? [NSRunningApplication runningApplicationWithProcessIdentifier:context.pid] : nil;
    valid = valid && [actualPath isEqualToString:context.executable] &&
        [application.bundleIdentifier isEqualToString:context.bundleID] &&
        [[application.bundleURL.path stringByResolvingSymlinksInPath] isEqualToString:context.bundle] &&
        [[application.executableURL.path stringByResolvingSymlinksInPath] isEqualToString:context.executable];
    if (!valid) {
        context.identityFailed = YES;
        barrier(context, @"identity_changed");
    }
    return valid;
}

static BOOL isElement(id object) {
    return object != nil && CFGetTypeID((__bridge CFTypeRef)object) == AXUIElementGetTypeID();
}

// Kernel-only diagnostic classification. This never grants an AX element read
// or write authority: prepare() below still requires the exact main PID.
static BOOL foreignProcessInOwnedBundle(UIContext *context, pid_t pid) {
    struct proc_bsdinfo before = {0}, after = {0};
    char path[PROC_PIDPATHINFO_MAXSIZE] = {0};
    if (pid <= 1 || proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &before, (int)sizeof(before)) != (int)sizeof(before) ||
        before.pbi_pid != (uint32_t)pid || before.pbi_uid != getuid() || before.pbi_start_tvsec == 0 ||
        proc_pidpath(pid, path, (uint32_t)sizeof(path)) <= 0 ||
        proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &after, (int)sizeof(after)) != (int)sizeof(after) ||
        after.pbi_pid != before.pbi_pid || after.pbi_uid != before.pbi_uid ||
        after.pbi_start_tvsec != before.pbi_start_tvsec || after.pbi_start_tvusec != before.pbi_start_tvusec) return NO;
    NSString *executable = [[NSString stringWithUTF8String:path] stringByResolvingSymlinksInPath];
    return [executable hasPrefix:[context.bundle stringByAppendingString:@"/"]];
}

static BOOL prepare(UIContext *context, AXUIElementRef element, BOOL restoring, BOOL writing) {
    double remaining = (restoring ? context.hardDeadline : context.normalDeadline) - monotonicNow();
    if (remaining < 0.02) {
        context.budgetExceeded = YES;
        barrier(context, @"deadline_exceeded");
        return NO;
    }
    pid_t owner = 0;
    if (element == NULL || AXUIElementGetPid(element, &owner) != kAXErrorSuccess || owner != context.pid) return NO;
    if (writing && !exactIdentity(context)) return NO;
    // Each IPC uses less than one second, and never consumes the restore reserve.
    Float32 timeout = (Float32)fmin(0.75, remaining / 2.0);
    return AXUIElementSetMessagingTimeout(element, timeout) == kAXErrorSuccess;
}

static id copyAttribute(UIContext *context, AXUIElementRef element, CFStringRef attribute,
                        BOOL restoring, AXError *error) {
    if (!prepare(context, element, restoring, NO)) {
        *error = kAXErrorCannotComplete;
        return nil;
    }
    CFTypeRef value = NULL;
    *error = AXUIElementCopyAttributeValue(element, attribute, &value);
    return CFBridgingRelease(value);
}

static BOOL copyBoolean(UIContext *context, AXUIElementRef element, CFStringRef attribute,
                        BOOL restoring, BOOL *value, AXError *error) {
    id object = copyAttribute(context, element, attribute, restoring, error);
    if (*error != kAXErrorSuccess || object == nil ||
        CFGetTypeID((__bridge CFTypeRef)object) != CFBooleanGetTypeID()) return NO;
    *value = CFBooleanGetValue((__bridge CFBooleanRef)object);
    return YES;
}

static BOOL settable(UIContext *context, AXUIElementRef element, CFStringRef attribute) {
    if (!prepare(context, element, NO, NO)) return NO;
    Boolean result = false;
    return AXUIElementIsAttributeSettable(element, attribute, &result) == kAXErrorSuccess && result;
}

static AXError writeBoolean(UIContext *context, AXUIElementRef element, CFStringRef attribute,
                             BOOL value, BOOL restoring) {
    if (!prepare(context, element, restoring, YES)) return kAXErrorCannotComplete;
    return AXUIElementSetAttributeValue(element, attribute, value ? kCFBooleanTrue : kCFBooleanFalse);
}

static BOOL waitBoolean(UIContext *context, AXUIElementRef element, CFStringRef attribute,
                         BOOL expected, BOOL restoring) {
    double limit = fmin(monotonicNow() + 2.0, restoring ? context.hardDeadline : context.normalDeadline);
    do {
        BOOL value = NO;
        AXError error = kAXErrorSuccess;
        if (copyBoolean(context, element, attribute, restoring, &value, &error) && value == expected) return YES;
        if (context.identityFailed || monotonicNow() >= limit) break;
        usleep(50000);
    } while (YES);
    return NO;
}

static NSMutableDictionary *controlReport(BOOL renderer) {
    NSMutableDictionary *result = [@{@"status": @"blocked", @"reason": @"not_attempted", @"attempted": @NO,
              (renderer ? @"focus_readback" : @"minimize_readback"): @NO, @"restored": @NO} mutableCopy];
    if (renderer) {
        result[@"focus_write_result"] = @"not_attempted";
        result[@"focus_attempts"] = @0;
        result[@"restore_write_result"] = @"not_attempted";
        result[@"restore_attempts"] = @0;
    }
    return result;
}

static NSMutableDictionary *newInspection(void) {
    NSDictionary *roles = @{@"window": @0, @"web_area": @0, @"button": @0, @"text_field": @0,
                            @"text_area": @0, @"static_text": @0, @"group": @0, @"other": @0,
                            @"scroll_area": @0, @"menu": @0, @"menu_item": @0, @"menu_bar": @0, @"unknown": @0};
    return [@{@"elements": @0, @"web_area_seen": @NO, @"truncated": @NO,
              @"role_counts": [roles mutableCopy], @"sign_in_label": @NO,
              @"onboarding_label": @NO, @"chat_label": @NO,
              @"owner_lookup_failures": @0, @"foreign_owner_elements": @0,
              @"owned_bundle_foreign_elements": @0} mutableCopy];
}

static NSMutableDictionary *newReport(void) {
    return [@{@"schema_version": @1, @"status": @"blocked", @"controller": @"native-ax",
              @"trusted": @NO, @"identity_verified": @NO, @"authentication": @"unknown",
              @"manual_accessibility": [@{@"supported": @NO, @"original_known": @NO, @"enabled": @NO,
                  @"enable_result": @"not_attempted", @"restored": @NO, @"restore_result": @"not_attempted"} mutableCopy],
              @"inspection": newInspection(),
              @"native_window_control": controlReport(NO), @"renderer_control": controlReport(YES),
              @"barriers": [NSMutableArray array], @"elapsed_seconds": @0} mutableCopy];
}

static BOOL unsignedNumber(NSString *text, uint64_t maximum, uint64_t *result) {
    if (text.length == 0 || [text rangeOfCharacterFromSet:[[NSCharacterSet decimalDigitCharacterSet] invertedSet]].location != NSNotFound) return NO;
    const char *raw = text.UTF8String;
    char *end = NULL;
    errno = 0;
    unsigned long long value = strtoull(raw, &end, 10);
    if (errno != 0 || end == raw || *end != '\0' || value > maximum) return NO;
    *result = (uint64_t)value;
    return YES;
}

static UIContext *arguments(int argc, const char *argv[]) {
    if (argc != 13) return nil;
    NSSet *keys = [NSSet setWithArray:@[@"--pid", @"--start-sec", @"--start-usec", @"--executable", @"--bundle", @"--bundle-id"]];
    NSMutableDictionary *values = [NSMutableDictionary dictionary];
    for (int index = 1; index < argc; index += 2) {
        NSString *key = [NSString stringWithUTF8String:argv[index]];
        NSString *value = [NSString stringWithUTF8String:argv[index + 1]];
        if (key == nil || value == nil || ![keys containsObject:key] || values[key] != nil) return nil;
        values[key] = value;
    }
    uint64_t pid = 0, sec = 0, usec = 0;
    if (!unsignedNumber(values[@"--pid"], INT_MAX, &pid) || pid <= 1 ||
        !unsignedNumber(values[@"--start-sec"], UINT64_MAX, &sec) || sec == 0 ||
        !unsignedNumber(values[@"--start-usec"], 999999, &usec)) return nil;
    NSString *bundle = values[@"--bundle"], *executable = values[@"--executable"];
    if (![bundle isAbsolutePath] || ![executable isAbsolutePath] ||
        ![bundle isEqualToString:[bundle stringByResolvingSymlinksInPath]] ||
        ![executable isEqualToString:[executable stringByResolvingSymlinksInPath]] ||
        ![executable hasPrefix:[bundle stringByAppendingString:@"/"]] ||
        ![values[@"--bundle-id"] isEqualToString:@"com.anthropic.claudefordesktop"]) return nil;
    UIContext *context = [UIContext new];
    context.pid = (pid_t)pid;
    context.startSec = sec;
    context.startUsec = usec;
    context.bundle = bundle;
    context.executable = executable;
    context.bundleID = values[@"--bundle-id"];
    return context;
}

static void recognizeLabel(NSString *label, NSMutableDictionary *inspection) {
    if ([@[@"Sign in", @"Log in", @"Continue with Google", @"Continue with email"] containsObject:label]) inspection[@"sign_in_label"] = @YES;
    if ([@[@"Get started", @"Get Started", @"Welcome to Claude"] containsObject:label]) inspection[@"onboarding_label"] = @YES;
    if ([@[@"Message Claude", @"Reply to Claude"] containsObject:label]) inspection[@"chat_label"] = @YES;
}

static BOOL visibleControl(UIContext *context, AXUIElementRef element) {
    BOOL enabled = NO, hidden = NO;
    AXError error = kAXErrorSuccess;
    if (!copyBoolean(context, element, kAXEnabledAttribute, NO, &enabled, &error) || !enabled) return NO;
    if (copyBoolean(context, element, CFSTR("AXHidden"), NO, &hidden, &error) && hidden) return NO;
    id position = copyAttribute(context, element, kAXPositionAttribute, NO, &error);
    id size = copyAttribute(context, element, kAXSizeAttribute, NO, &error);
    CGPoint point = CGPointZero;
    CGSize dimensions = CGSizeZero;
    if (position == nil || size == nil || CFGetTypeID((__bridge CFTypeRef)position) != AXValueGetTypeID() ||
        CFGetTypeID((__bridge CFTypeRef)size) != AXValueGetTypeID() ||
        !AXValueGetValue((__bridge AXValueRef)position, kAXValueCGPointType, &point) ||
        !AXValueGetValue((__bridge AXValueRef)size, kAXValueCGSizeType, &dimensions) ||
        dimensions.width <= 0 || dimensions.height <= 0) return NO;
    return CGRectIntersectsRect(CGRectMake(point.x, point.y, dimensions.width, dimensions.height), CGDisplayBounds(CGMainDisplayID()));
}

static id ownedWindow(UIContext *context, AXUIElementRef application) {
    AXError error = kAXErrorSuccess;
    id windows = copyAttribute(context, application, kAXWindowsAttribute, NO, &error);
    if (error != kAXErrorSuccess || ![windows isKindOfClass:[NSArray class]] || [windows count] != 1 || !isElement([windows firstObject])) return nil;
    id window = [windows firstObject];
    if (!prepare(context, (__bridge AXUIElementRef)window, NO, NO)) return nil;
    id role = copyAttribute(context, (__bridge AXUIElementRef)window, kAXRoleAttribute, NO, &error);
    return error == kAXErrorSuccess && [role isKindOfClass:[NSString class]] && [role isEqualToString:@"AXWindow"] ? window : nil;
}

static id waitOwnedWindow(UIContext *context, AXUIElementRef application) {
    double deadline = fmin(monotonicNow() + 8, context.normalDeadline);
    do {
        // Startup readiness is read-only. Never enable AX or act on UI while
        // the app's kernel/bundle identity or unique owned window is unknown.
        if (!exactIdentity(context)) return nil;
        id window = ownedWindow(context, application);
        if (window != nil) return window;
        double remaining = deadline - monotonicNow();
        if (remaining <= 0) return nil;
        usleep((useconds_t)(fmin(0.2, remaining) * 1000000));
    } while (YES);
}

static NSArray *inspect(UIContext *context, AXUIElementRef application, NSMutableDictionary *inspection, double inspectionDeadline) {
    id window = ownedWindow(context, application);
    if (window == nil) {
        inspection[@"truncated"] = @YES;
        barrier(context, @"inspection_incomplete");
        return @[];
    }
    // Application-global menus can exhaust the budget before renderer content.
    // Inspect only the exact owned window that the native control proof uses.
    NSMutableArray *queue = [NSMutableArray arrayWithObject:@{@"element": window, @"depth": @0, @"web": @NO}];
    NSMutableArray *candidates = [NSMutableArray array];
    NSMutableArray *countedOwners = [NSMutableArray array];
    NSDictionary *roleKeys = @{@"AXWindow": @"window", @"AXWebArea": @"web_area", @"AXButton": @"button",
        @"AXTextField": @"text_field", @"AXTextArea": @"text_area", @"AXStaticText": @"static_text", @"AXGroup": @"group",
        @"AXScrollArea": @"scroll_area", @"AXMenu": @"menu", @"AXMenuItem": @"menu_item", @"AXMenuBar": @"menu_bar",
        @"AXUnknown": @"unknown"};
    NSUInteger cursor = 0;
    while (cursor < queue.count && [inspection[@"elements"] unsignedIntegerValue] < MaxInspectionElements && monotonicNow() < inspectionDeadline) {
        NSDictionary *node = queue[cursor++];
        id object = node[@"element"];
        if (!isElement(object)) continue;
        AXUIElementRef element = (__bridge AXUIElementRef)object;
        BOOL firstVisit = YES;
        for (id seen in countedOwners) {
            if (CFEqual((__bridge CFTypeRef)seen, (__bridge CFTypeRef)object)) { firstVisit = NO; break; }
        }
        if (firstVisit) [countedOwners addObject:object];
        pid_t owner = 0;
        AXError ownerResult = AXUIElementGetPid(element, &owner);
        if (ownerResult != kAXErrorSuccess || owner <= 1) {
            if (firstVisit) inspection[@"owner_lookup_failures"] = @([inspection[@"owner_lookup_failures"] unsignedIntegerValue] + 1);
            continue;
        }
        if (owner != context.pid) {
            if (firstVisit) {
                inspection[@"foreign_owner_elements"] = @([inspection[@"foreign_owner_elements"] unsignedIntegerValue] + 1);
                if (foreignProcessInOwnedBundle(context, owner)) inspection[@"owned_bundle_foreign_elements"] =
                    @([inspection[@"owned_bundle_foreign_elements"] unsignedIntegerValue] + 1);
            }
            continue; // Diagnostic metadata never broadens the AX ownership policy.
        }
        if (!prepare(context, element, NO, NO)) continue;
        inspection[@"elements"] = @([inspection[@"elements"] unsignedIntegerValue] + 1);
        AXError error = kAXErrorSuccess;
        id roleObject = copyAttribute(context, element, kAXRoleAttribute, NO, &error);
        NSString *role = [roleObject isKindOfClass:[NSString class]] ? roleObject : @"";
        NSString *key = role.length == 0 ? @"unknown" : (roleKeys[role] ?: @"other");
        NSMutableDictionary *counts = inspection[@"role_counts"];
        counts[key] = @([counts[key] unsignedIntegerValue] + 1);
        BOOL web = [node[@"web"] boolValue] || [role isEqualToString:@"AXWebArea"];
        if ([role isEqualToString:@"AXWebArea"]) inspection[@"web_area_seen"] = @YES;
        if ([@[@"AXButton", @"AXStaticText", @"AXTextField", @"AXTextArea"] containsObject:role]) {
            for (id attribute in @[(__bridge id)kAXTitleAttribute, (__bridge id)kAXDescriptionAttribute]) {
                id label = copyAttribute(context, element, (__bridge CFStringRef)attribute, NO, &error);
                if ([label isKindOfClass:[NSString class]]) recognizeLabel(label, inspection);
            }
        }
        if (web && [@[@"AXButton", @"AXTextField"] containsObject:role] && visibleControl(context, element)) [candidates addObject:object];
        NSUInteger depth = [node[@"depth"] unsignedIntegerValue];
        if (depth >= MaxInspectionDepth) { inspection[@"truncated"] = @YES; continue; }
        CFIndex count = 0;
        if (!prepare(context, element, NO, NO) || AXUIElementGetAttributeValueCount(element, kAXChildrenAttribute, &count) != kAXErrorSuccess || count <= 0) continue;
        CFIndex limit = (CFIndex)MIN((NSUInteger)count, MaxInspectionElements - queue.count);
        if (limit <= 0) { inspection[@"truncated"] = @YES; continue; }
        if (limit < count) inspection[@"truncated"] = @YES;
        CFArrayRef children = NULL;
        if (!prepare(context, element, NO, NO) || AXUIElementCopyAttributeValues(element, kAXChildrenAttribute, 0, limit, &children) != kAXErrorSuccess) continue;
        NSArray *values = CFBridgingRelease(children);
        for (id child in values) {
            if (isElement(child)) [queue addObject:@{@"element": child, @"depth": @(depth + 1), @"web": @(web)}];
        }
    }
    if (cursor < queue.count) inspection[@"truncated"] = @YES;
    if ([inspection[@"truncated"] boolValue]) barrier(context, @"inspection_incomplete");
    return candidates;
}

static void nativeWindow(UIContext *context, AXUIElementRef application, NSMutableDictionary *result) {
    AXError error = kAXErrorSuccess;
    id object = ownedWindow(context, application);
    if (object == nil) { result[@"reason"] = @"no_owned_window"; return; }
    AXUIElementRef window = (__bridge AXUIElementRef)object;
    BOOL original = NO;
    if (!copyBoolean(context, window, kAXMinimizedAttribute, NO, &original, &error)) { result[@"reason"] = @"window_state_unknown"; return; }
    if (original) { result[@"reason"] = @"window_already_minimized"; return; }
    if (!settable(context, window, kAXMinimizedAttribute)) { result[@"reason"] = @"window_not_settable"; return; }
    result[@"attempted"] = @YES;
    result[@"status"] = @"failed";
    @try {
        if (writeBoolean(context, window, kAXMinimizedAttribute, YES, NO) != kAXErrorSuccess) { result[@"reason"] = @"minimize_write_failed"; return; }
        if (!waitBoolean(context, window, kAXMinimizedAttribute, YES, NO)) { result[@"reason"] = @"minimize_readback_failed"; return; }
        result[@"minimize_readback"] = @YES;
        result[@"reason"] = @"verified";
    } @finally {
        BOOL restored = writeBoolean(context, window, kAXMinimizedAttribute, original, YES) == kAXErrorSuccess &&
            waitBoolean(context, window, kAXMinimizedAttribute, original, YES);
        result[@"restored"] = @(restored);
        if (!restored) result[@"reason"] = @"restore_failed";
        if (restored && [result[@"minimize_readback"] boolValue]) result[@"status"] = @"passed";
    }
}

static BOOL focusedEquals(UIContext *context, AXUIElementRef application, AXUIElementRef expected, BOOL restoring) {
    double limit = fmin(monotonicNow() + 2.0, restoring ? context.hardDeadline : context.normalDeadline);
    do {
        AXError error = kAXErrorSuccess;
        id focus = copyAttribute(context, application, kAXFocusedUIElementAttribute, restoring, &error);
        BOOL value = NO;
        if (isElement(focus) && CFEqual((__bridge AXUIElementRef)focus, expected) &&
            copyBoolean(context, expected, kAXFocusedAttribute, restoring, &value, &error) && value) return YES;
        if (context.identityFailed || monotonicNow() >= limit) break;
        usleep(50000);
    } while (YES);
    return NO;
}

static BOOL setAndVerifyRendererFocus(UIContext *context, AXUIElementRef application, AXUIElementRef target,
                                       BOOL restoring, NSMutableDictionary *result) {
    // Both actuation and rollback retry only their exact captured target. Each
    // phase has its own eight-second sub-budget and keeps its original deadline.
    double originalDeadline = restoring ? context.hardDeadline : context.normalDeadline;
    double deadline = fmin(originalDeadline, monotonicNow() + 8.0);
    if (restoring) context.hardDeadline = deadline;
    else context.normalDeadline = deadline;
    NSString *attemptsKey = restoring ? @"restore_attempts" : @"focus_attempts";
    NSString *writeKey = restoring ? @"restore_write_result" : @"focus_write_result";
    BOOL verified = NO;
    @try {
        for (NSUInteger attempt = 1; attempt <= MaxFocusAttempts; attempt++) {
            if (context.identityFailed || deadline - monotonicNow() < 0.02) break;
            AXError error = writeBoolean(context, target, kAXFocusedAttribute, YES, restoring);
            result[attemptsKey] = @(attempt);
            result[writeKey] = axName(error);
            if (error == kAXErrorSuccess && focusedEquals(context, application, target, restoring)) {
                verified = YES;
                break;
            }
            // An invalid captured AX element cannot be replaced by another
            // element with a matching label/role.
            if (error == kAXErrorInvalidUIElement || context.identityFailed) break;
            double remaining = deadline - monotonicNow();
            if (remaining <= 0) break;
            usleep((useconds_t)(fmin(0.25, remaining) * 1000000));
        }
    } @finally {
        if (restoring) context.hardDeadline = originalDeadline;
        else context.normalDeadline = originalDeadline;
    }
    return verified;
}

static void rendererFocus(UIContext *context, AXUIElementRef application, NSArray *candidates,
                          NSMutableDictionary *inspection, NSMutableDictionary *result) {
    if (![inspection[@"web_area_seen"] boolValue]) { result[@"reason"] = @"no_web_area"; return; }
    AXError error = kAXErrorSuccess;
    id original = copyAttribute(context, application, kAXFocusedUIElementAttribute, NO, &error);
    if (!isElement(original) || !prepare(context, (__bridge AXUIElementRef)original, NO, NO) ||
        !settable(context, (__bridge AXUIElementRef)original, kAXFocusedAttribute) ||
        !focusedEquals(context, application, (__bridge AXUIElementRef)original, NO)) { result[@"reason"] = @"no_original_focus"; return; }
    id selected = nil;
    for (id candidate in candidates) {
        if (CFEqual((__bridge AXUIElementRef)candidate, (__bridge AXUIElementRef)original)) continue;
        if (settable(context, (__bridge AXUIElementRef)candidate, kAXFocusedAttribute) &&
            visibleControl(context, (__bridge AXUIElementRef)candidate)) { selected = candidate; break; }
    }
    if (selected == nil) { result[@"reason"] = @"no_distinct_focus_candidate"; return; }
    result[@"attempted"] = @YES;
    result[@"status"] = @"failed";
    @try {
        if (!setAndVerifyRendererFocus(context, application, (__bridge AXUIElementRef)selected, NO, result)) {
            result[@"reason"] = [result[@"focus_write_result"] isEqualToString:@"success"] ? @"focus_readback_failed" : @"focus_write_failed";
            return;
        }
        result[@"focus_readback"] = @YES;
        result[@"reason"] = @"verified";
    } @finally {
        BOOL restored = setAndVerifyRendererFocus(context, application, (__bridge AXUIElementRef)original, YES, result);
        result[@"restored"] = @(restored);
        if (!restored) result[@"reason"] = @"restore_failed";
        if (restored && [result[@"focus_readback"] boolValue]) result[@"status"] = @"passed";
    }
}

static void exercise(UIContext *context, NSMutableDictionary *report) {
    context.barriers = report[@"barriers"];
    if (!exactIdentity(context)) { report[@"status"] = @"failed"; return; }
    report[@"identity_verified"] = @YES;
    BOOL trusted = AXIsProcessTrusted(); // No prompt and no trust database changes.
    report[@"trusted"] = @(trusted);
    if (!trusted) {
        barrier(context, @"accessibility_not_trusted");
        report[@"native_window_control"][@"reason"] = @"accessibility_not_trusted";
        report[@"renderer_control"][@"reason"] = @"accessibility_not_trusted";
        return;
    }
    id applicationObject = CFBridgingRelease(AXUIElementCreateApplication(context.pid));
    AXUIElementRef application = (__bridge AXUIElementRef)applicationObject;
    if (waitOwnedWindow(context, application) == nil) {
        BOOL identityChanged = context.identityFailed;
        report[@"identity_verified"] = identityChanged ? @NO : @YES;
        report[@"status"] = identityChanged ? @"failed" : @"blocked";
        NSString *reason = identityChanged ? @"identity_changed" : @"no_unique_window";
        report[@"native_window_control"][@"reason"] = reason;
        report[@"renderer_control"][@"reason"] = reason;
        barrier(context, @"native_window_control_blocked");
        barrier(context, @"renderer_control_blocked");
        return;
    }
    NSMutableDictionary *manual = report[@"manual_accessibility"];
    BOOL original = NO, changed = NO;
    AXError error = kAXErrorSuccess;
    BOOL known = copyBoolean(context, application, CFSTR("AXManualAccessibility"), NO, &original, &error);
    manual[@"original_known"] = @(known);
    manual[@"supported"] = @(known);
    manual[@"enable_result"] = known ? @"not_attempted" : axName(error);
    if (!known) {
        manual[@"restore_result"] = @"original_unknown";
        barrier(context, @"manual_accessibility_unavailable");
    }
    @try {
        if (known && original) {
            manual[@"enabled"] = @YES;
            manual[@"enable_result"] = @"already_enabled";
            manual[@"restored"] = @YES;
            manual[@"restore_result"] = @"unchanged";
        } else if (known) {
            // Even failed IPC may have delivered a write; always try rollback.
            changed = YES;
            error = writeBoolean(context, application, CFSTR("AXManualAccessibility"), YES, NO);
            manual[@"enable_result"] = axName(error);
            // Electron debounces complete screen-reader mode by two seconds.
            // AX reads during that interval can restart its delayed work.
            BOOL quietPeriodComplete = NO;
            if (error == kAXErrorSuccess) {
                if (context.normalDeadline - monotonicNow() >= 2.25) {
                    [NSThread sleepForTimeInterval:2.25];
                    quietPeriodComplete = YES;
                } else {
                    context.budgetExceeded = YES;
                    barrier(context, @"deadline_exceeded");
                }
            }
            // C logical operators produce int. Box an explicit JSON Boolean,
            // rather than serializing a successful readback as numeric 1.
            BOOL enabled = error == kAXErrorSuccess && quietPeriodComplete &&
                waitBoolean(context, application, CFSTR("AXManualAccessibility"), YES, NO);
            manual[@"enabled"] = enabled ? @YES : @NO;
            if (![manual[@"enabled"] boolValue]) barrier(context, @"manual_accessibility_unavailable");
        }
        NSMutableDictionary *inspection = nil;
        NSArray *candidates = nil;
        // With no external startup polling, renderer creation may take most
        // of the startup budget. Reserve normal-operation time for both real
        // control proofs, and keep the separate rollback reserve untouched.
        double inspectionDeadline = context.normalDeadline - 8;
        // Electron exposes renderer AX asynchronously after enabling it. Each
        // snapshot owns fresh bounded counters, never an accumulated raw tree.
        do {
            if (inspection != nil && monotonicNow() >= inspectionDeadline) break;
            NSMutableDictionary *snapshot = newInspection();
            NSArray *snapshotCandidates = inspect(context, application, snapshot, inspectionDeadline);
            // Scheduling or the last sleep may exhaust the deadline before a
            // new traversal starts. Keep the last real bounded observation.
            if (inspection == nil || [snapshot[@"elements"] unsignedIntegerValue] > 0) {
                inspection = snapshot;
                candidates = snapshotCandidates;
                report[@"inspection"] = inspection;
            }
            if ([snapshot[@"web_area_seen"] boolValue] && snapshotCandidates.count > 0) break;
            if (monotonicNow() >= inspectionDeadline || context.identityFailed) break;
            double delay = fmin(0.2, inspectionDeadline - monotonicNow());
            if (delay > 0) usleep((useconds_t)(delay * 1000000));
        } while (YES);
        if ([inspection[@"sign_in_label"] boolValue]) report[@"authentication"] = @"sign_in_required";
        // Inspect/restore genuine current renderer focus before a native
        // minimize cycle can independently affect the application's focus.
        rendererFocus(context, application, candidates, inspection, report[@"renderer_control"]);
        nativeWindow(context, application, report[@"native_window_control"]);
    } @finally {
        if (changed) {
            error = writeBoolean(context, application, CFSTR("AXManualAccessibility"), original, YES);
            manual[@"restore_result"] = axName(error);
            BOOL restored = error == kAXErrorSuccess && waitBoolean(context, application, CFSTR("AXManualAccessibility"), original, YES);
            manual[@"restored"] = @(restored);
            if (!restored) barrier(context, @"manual_accessibility_restore_failed");
        }
    }
    if (!exactIdentity(context)) report[@"identity_verified"] = @NO;
    BOOL failed = context.identityFailed || (changed && ![manual[@"restored"] boolValue]);
    BOOL passed = YES;
    for (NSString *key in @[@"native_window_control", @"renderer_control"]) {
        NSString *status = report[key][@"status"];
        NSString *prefix = [key isEqualToString:@"native_window_control"] ? @"native_window_control" : @"renderer_control";
        if ([status isEqualToString:@"failed"]) { failed = YES; barrier(context, [prefix stringByAppendingString:@"_failed"]); }
        if (![status isEqualToString:@"passed"]) { passed = NO; if ([status isEqualToString:@"blocked"]) barrier(context, [prefix stringByAppendingString:@"_blocked"]); }
    }
    report[@"status"] = failed ? @"failed" : (passed ? @"passed" : @"blocked");
}

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        double started = monotonicNow();
        NSMutableDictionary *report = newReport();
        UIContext *context = arguments(argc, argv);
        if (context == nil) {
            report[@"status"] = @"failed";
            [report[@"barriers"] addObject:@"invalid_arguments"];
        } else {
            // Normal work stops after forty-five seconds, reserving thirteen for
            // guarded rollback; every AX IPC has its own subsecond timeout.
            context.normalDeadline = started + 45;
            context.hardDeadline = started + 58;
            @try {
                exercise(context, report);
            } @catch (__unused NSException *exception) {
                report[@"status"] = @"failed";
                [report[@"barriers"] addObject:@"internal_failure"];
            }
        }
        report[@"elapsed_seconds"] = @(round((monotonicNow() - started) * 1000) / 1000);
        NSError *error = nil;
        NSData *json = [NSJSONSerialization dataWithJSONObject:report options:NSJSONWritingSortedKeys error:&error];
        if (json == nil || error != nil) {
            fputs("{\"schema_version\":1,\"status\":\"failed\",\"controller\":\"native-ax\",\"barriers\":[\"internal_failure\"]}\n", stdout);
            return 1;
        }
        fwrite(json.bytes, 1, json.length, stdout);
        fputc('\n', stdout);
        return [report[@"status"] isEqualToString:@"failed"] ? 1 : 0;
    }
}
