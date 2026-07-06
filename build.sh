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
    GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
        go build -ldflags="$LDFLAGS -H windowsgui" -o build/grailward-agent-windows-amd64.exe
else
    echo "Warning: x86_64-w64-mingw32-gcc not found — skipping Windows build."
    echo "         Install mingw-w64 (brew install mingw-w64) or build on Windows."
fi

echo "=== Build Complete ==="
echo "Artifacts generated in agent/build/:"
ls -lh build/
