#!/bin/bash
set -e

# Change directory to the script's directory (agent/)
cd "$(dirname "$0")"

# Version stamped into the binary (main.Version). Overridden by release.sh;
# a bare `./build.sh` produces a "dev" build.
VERSION="${VERSION:-dev}"
LDFLAGS="-s -w -X main.Version=${VERSION}"

# systray needs CGO (Cocoa on macOS, Win32 on Windows), so the pure-Go
# CGO_ENABLED=0 cross-compile no longer applies. macOS builds natively here
# (universal via clang -arch); Windows requires a mingw cross toolchain.
export CGO_ENABLED=1

echo "=== Starting Build Process (version: ${VERSION}) ==="

mkdir -p build
rm -f build/grailward-agent-*
rm -rf build/Grailward\ Agent.app
# A stale Windows resource object from an aborted run would get linked into the
# next windows/amd64 build; clear it up front (it is gitignored either way).
rm -f rsrc_windows_amd64.syso

# 1. macOS universal binary (amd64 + arm64 via the system clang).
echo "Building macOS AMD64..."
GOOS=darwin GOARCH=amd64 CC="clang -arch x86_64" go build -ldflags="$LDFLAGS" -o build/grailward-agent-darwin-amd64

echo "Building macOS ARM64..."
GOOS=darwin GOARCH=arm64 CC="clang -arch arm64" go build -ldflags="$LDFLAGS" -o build/grailward-agent-darwin-arm64

if command -v lipo >/dev/null 2>&1; then
    echo "Creating macOS Universal Binary using lipo..."
    lipo -create -output build/grailward-agent-darwin build/grailward-agent-darwin-amd64 build/grailward-agent-darwin-arm64
    echo "macOS Universal Binary created: build/grailward-agent-darwin"

    # 2. Wrap into a .app bundle. LSUIElement=true makes it a menu-bar-only
    #    agent (no Dock icon, no terminal window).
    APP="build/Grailward Agent.app"
    mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
    cp build/grailward-agent-darwin "$APP/Contents/MacOS/grailward-agent"

    # 2a. Give the bundle a Finder icon (AppIcon.icns), built from the vendored
    #     512px app art. Purely cosmetic — if the macOS icon tools are missing,
    #     warn and ship the bundle without one rather than fail the build.
    ICON_PLIST_KEYS=""
    if command -v sips >/dev/null 2>&1 && command -v iconutil >/dev/null 2>&1; then
        echo "Generating AppIcon.icns from icons/app.png..."
        ICONSET="build/AppIcon.iconset"
        rm -rf "$ICONSET"
        mkdir -p "$ICONSET"
        # Standard iconset slots. The source is 512px, so the largest slot is
        # 512 (icon_256x256@2x); no upscaling is done.
        sips -z 16 16   icons/app.png --out "$ICONSET/icon_16x16.png"      >/dev/null
        sips -z 32 32   icons/app.png --out "$ICONSET/icon_16x16@2x.png"   >/dev/null
        sips -z 32 32   icons/app.png --out "$ICONSET/icon_32x32.png"      >/dev/null
        sips -z 64 64   icons/app.png --out "$ICONSET/icon_32x32@2x.png"   >/dev/null
        sips -z 128 128 icons/app.png --out "$ICONSET/icon_128x128.png"    >/dev/null
        sips -z 256 256 icons/app.png --out "$ICONSET/icon_128x128@2x.png" >/dev/null
        sips -z 256 256 icons/app.png --out "$ICONSET/icon_256x256.png"    >/dev/null
        sips -z 512 512 icons/app.png --out "$ICONSET/icon_256x256@2x.png" >/dev/null
        sips -z 512 512 icons/app.png --out "$ICONSET/icon_512x512.png"    >/dev/null
        iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"
        rm -rf "$ICONSET"
        ICON_PLIST_KEYS="    <key>CFBundleIconFile</key><string>AppIcon</string>
    <key>CFBundleIconName</key><string>AppIcon</string>"
        echo "AppIcon.icns embedded in the bundle."
    else
        echo "Warning: 'sips'/'iconutil' not found — .app bundle will have no Finder icon."
    fi

    cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleName</key><string>Grailward Agent</string>
    <key>CFBundleIdentifier</key><string>com.grailward.agent</string>
    <key>CFBundleExecutable</key><string>grailward-agent</string>
    <key>CFBundleVersion</key><string>${VERSION}</string>
    <key>CFBundleShortVersionString</key><string>${VERSION}</string>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>LSUIElement</key><true/>
    <key>LSMinimumSystemVersion</key><string>10.15</string>
${ICON_PLIST_KEYS}
</dict>
</plist>
PLIST
    echo "macOS .app bundle created: $APP"

    # 2b. Zip the bundle for distribution — R2/HTTP serve single files, and a
    #     .app is a directory. This zip is the macOS user download.
    if command -v ditto >/dev/null 2>&1; then
        (cd build && ditto -c -k --keepParent "Grailward Agent.app" grailward-agent-macos.zip)
    else
        (cd build && zip -qry grailward-agent-macos.zip "Grailward Agent.app")
    fi
    echo "macOS distribution zip created: build/grailward-agent-macos.zip"
else
    echo "Warning: 'lipo' not found, skipping universal binary and .app bundle."
fi

# 3. Windows AMD64 — only if a mingw cross toolchain is available.
#    -H windowsgui hides the console window (background tray app).
if command -v x86_64-w64-mingw32-gcc >/dev/null 2>&1; then
    echo "Building Windows AMD64..."

    # Embed a PE resource (app icon + version metadata) so the .exe shows the
    # grailward shield in Explorer instead of the generic file icon, and carries
    # a Details tab. windres compiles a .rc into a COFF object named
    # rsrc_windows_amd64.syso in the package dir; `go build` links any .syso whose
    # name matches the target GOOS/GOARCH automatically. Missing tools => warn and
    # build a resource-less .exe rather than fail.
    if command -v x86_64-w64-mingw32-windres >/dev/null 2>&1 && command -v sips >/dev/null 2>&1; then
        echo "  Building Windows icon + version resource..."
        WINRES="build/winres"
        rm -rf "$WINRES"
        mkdir -p "$WINRES"
        # Downscale the 512px app art into the icon sizes Explorer picks from.
        for s in 16 32 48 64 256; do
            sips -z "$s" "$s" icons/app.png --out "$WINRES/app-$s.png" >/dev/null
        done
        # Pack them into one multi-entry ICO (PNG entries, Vista+).
        go run ./tools/mkico -o build/app.ico \
            "$WINRES/app-16.png" "$WINRES/app-32.png" "$WINRES/app-48.png" \
            "$WINRES/app-64.png" "$WINRES/app-256.png"

        # FILE/PRODUCTVERSION must be four integers. Strip a leading "v", take the
        # numeric dotted prefix, and pad to X,Y,Z,W; "dev" (or anything without a
        # numeric prefix) collapses to 0,0,0,0. The human-readable strings keep the
        # raw $VERSION value verbatim.
        vnum=$(printf '%s' "$VERSION" | sed 's/^v//' | grep -Eo '^[0-9]+(\.[0-9]+)*' || true)
        IFS='.' read -r v1 v2 v3 v4 _ <<VER
$vnum
VER
        COMMA_VERSION="${v1:-0},${v2:-0},${v3:-0},${v4:-0}"

        cat > build/app.rc <<RC
IDI_ICON1 ICON "app.ico"

1 VERSIONINFO
FILEVERSION $COMMA_VERSION
PRODUCTVERSION $COMMA_VERSION
FILEOS 0x40004
FILETYPE 0x1
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "040904b0"
    BEGIN
      VALUE "CompanyName", "grailward.com"
      VALUE "FileDescription", "Grailward Agent"
      VALUE "FileVersion", "$VERSION"
      VALUE "ProductName", "Grailward Agent"
      VALUE "ProductVersion", "$VERSION"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x409, 1200
  END
END
RC
        # -I build lets windres resolve the ICON's "app.ico" (which lives in build/).
        x86_64-w64-mingw32-windres -I build -i build/app.rc -O coff -o rsrc_windows_amd64.syso
    else
        echo "  Warning: windres/sips not found — Windows .exe will have no icon/version resource."
    fi

    GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
        go build -ldflags="$LDFLAGS -H windowsgui" -o build/grailward-agent-windows-amd64.exe

    # The .syso is a transient build input in the package dir — remove it so it
    # never lingers or leaks into a later run.
    rm -f rsrc_windows_amd64.syso
    rm -rf build/winres build/app.rc
else
    echo "Warning: x86_64-w64-mingw32-gcc not found — skipping Windows build."
    echo "         Install mingw-w64 (brew install mingw-w64) or build on Windows."
fi

echo "=== Build Complete ==="
echo "Artifacts generated in agent/build/:"
ls -lh build/
