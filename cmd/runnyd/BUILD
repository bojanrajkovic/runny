load("@rules_go//go:def.bzl", "go_library", "go_test")
load("//tools:stamp.bzl", "stamped_go_binary")
load("//tools/sign:sign.bzl", "codesign_binary", "notarize_binary")

# gazelle:map_kind go_binary stamped_go_binary //tools:stamp.bzl

go_library(
    name = "runnyd_lib",
    srcs = [
        "competing.go",
        "dockerconfig.go",
        "drain.go",
        "fileowner_unix.go",
        "fileowner_windows.go",
        "hostprobe_darwin.go",
        "hostprobe_other.go",
        "hostprobe_windows.go",
        "launchcontext.go",
        "launchcontext_darwin.go",
        "launchcontext_other.go",
        "launchcontext_windows.go",
        "localnet.go",
        "lock.go",
        "lock_unix.go",
        "lock_windows.go",
        "main.go",
        "parsegate.go",
        "platform_darwin.go",
        "platform_other.go",
        "platform_windows.go",
        "privatekey.go",
        "rotate.go",
        "svc_other.go",
        "svc_windows.go",
        "testconfig.go",
        "upgrade.go",
    ],
    importpath = "github.com/bojanrajkovic/runny/cmd/runnyd",
    visibility = ["//visibility:private"],
    deps = [
        "//internal/bounded",
        "//internal/clonefile",
        "//internal/cycle",
        "//internal/diskfree",
        "//internal/github",
        "//internal/guest",
        "//internal/home",
        "//internal/images",
        "//internal/launchd",
        "//internal/logring",
        "//internal/obs",
        "//internal/oci",
        "//internal/respawn",
        "//internal/socket",
        "//internal/sshx",
        "//internal/statemachine",
        "//internal/sysdaemon",
        "//internal/tart",
        "//internal/telemetry",
        "//internal/testconfig",
        "//internal/versioncore",
        "//internal/vhdx",
        "//proto/runny/v1:runny",
        "@io_opentelemetry_go_otel//:otel",
    ] + select({
        "@rules_go//go/platform:aix": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:android": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:darwin": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:dragonfly": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:freebsd": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:illumos": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:ios": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:js": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:linux": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:netbsd": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:openbsd": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:osx": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:plan9": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:qnx": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:solaris": [
            "//internal/vm",
            "@org_golang_x_sys//unix",
        ],
        "@rules_go//go/platform:windows": [
            "//internal/vm",
            "//internal/winhcs/hcn",
            "//internal/winhcs/osversion",
            "@org_golang_x_sys//windows",
            "@org_golang_x_sys//windows/svc",
        ],
        "//conditions:default": [],
    }),
)

stamped_go_binary(
    name = "runnyd",
    embed = [":runnyd_lib"],
    visibility = ["//visibility:public"],
)

codesign_binary(
    name = "runnyd_signed",
    binary = ":runnyd",
    entitlement_key = "com.apple.security.virtualization",
    entitlements = "//tools/sign:runnyd.entitlements",
    visibility = ["//visibility:public"],
)

notarize_binary(
    name = "runnyd_notarized",
    binary = ":runnyd_signed",
    visibility = ["//visibility:public"],
)

go_test(
    name = "runnyd_test",
    srcs = [
        "competing_test.go",
        "dockerconfig_test.go",
        "drain_test.go",
        "installassert_test.go",
        "launchcontext_test.go",
        "localnet_test.go",
        "lock_test.go",
        "privatekey_test.go",
        "reload_test.go",
        "rotate_test.go",
        "svc_windows_test.go",
        "sweepvmsdir_test.go",
        "testconfig_test.go",
        "upgrade_test.go",
    ],
    embed = [":runnyd_lib"],
    deps = [
        "//internal/home",
        "//internal/launchd",
        "//internal/oci",
        "//internal/socket",
        "//internal/statemachine",
        "//internal/sysdaemon",
        "//proto/runny/v1:runny",
    ] + select({
        "@rules_go//go/platform:windows": [
            "@org_golang_x_sys//windows/svc",
        ],
        "//conditions:default": [],
    }),
)
