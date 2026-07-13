package main

import (
	_ "embed"
	"encoding/binary"
	"runtime"
)

//go:embed icons/syncing.png
var pngSyncing []byte

//go:embed icons/paused.png
var pngPaused []byte

//go:embed icons/error.png
var pngError []byte

//go:embed icons/transferring.png
var pngTransferring []byte

// Tray icon bytes in the encoding the host platform's systray backend expects.
// The fyne.io/systray backends on macOS and Linux take a raw PNG, but the Win32
// backend only accepts the ICO container format (a PNG hands it a blank icon).
// The conversion happens once here at package init rather than on every SetIcon
// call. runtime.GOOS is a compile-time constant, so on macOS the non-Windows
// branch is all that survives and these hold the embedded PNG bytes verbatim —
// the macOS behavior is unchanged.
var (
	iconSyncing = trayIcon(pngSyncing)
	iconPaused  = trayIcon(pngPaused)
	iconError   = trayIcon(pngError)
	// iconTransferring is the activity badge (a circle of arrows) shown while a
	// real transfer batch is in flight, distinct from the idle "watching" state.
	iconTransferring = trayIcon(pngTransferring)
)

// iconForState maps a coarse run state to its tray icon bytes. It is a pure
// lookup so the tray's icon decisions stay unit-testable without a live systray.
func iconForState(state State) []byte {
	switch state {
	case StatePaused:
		return iconPaused
	case StateError:
		return iconError
	default: // StateSyncing
		return iconSyncing
	}
}

// trayIcon returns the platform-appropriate encoding of a tray icon.
func trayIcon(png []byte) []byte {
	if runtime.GOOS == "windows" {
		return pngToICO(png)
	}
	return png
}

// pngToICO wraps a PNG in a single-image ICO container. Windows Vista and later
// accept a PNG-compressed image stored directly inside an ICO, so no re-encoding
// is needed: a 6-byte ICONDIR, a 16-byte ICONDIRENTRY, then the PNG payload
// appended verbatim.
//
// The entry's width and height are single bytes in which 0 encodes 256. They are
// read from the PNG's IHDR (width at offset 16, height at offset 20, each a
// big-endian uint32) and folded down to the 0 sentinel when 256 or larger.
func pngToICO(png []byte) []byte {
	var w, h byte
	if len(png) >= 24 {
		if width := binary.BigEndian.Uint32(png[16:20]); width < 256 {
			w = byte(width)
		}
		if height := binary.BigEndian.Uint32(png[20:24]); height < 256 {
			h = byte(height)
		}
	}

	// ICONDIR (6 bytes) + one ICONDIRENTRY (16 bytes); the image blob starts here.
	const headerSize = 6 + 16

	out := make([]byte, 0, headerSize+len(png))
	// ICONDIR: reserved=0, image type=1 (icon), image count=1 (each uint16 LE).
	out = append(out, 0, 0, 1, 0, 1, 0)
	// ICONDIRENTRY.
	out = append(out,
		w,    // width  (0 => 256)
		h,    // height (0 => 256)
		0,    // palette color count (0 => no palette)
		0,    // reserved
		1, 0, // color planes  (uint16 LE)
		32, 0, // bits per pixel (uint16 LE)
	)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(png))) // bytes in resource
	out = binary.LittleEndian.AppendUint32(out, headerSize)       // offset to the blob
	return append(out, png...)
}
