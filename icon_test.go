package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// synthPNG builds a minimal synthetic PNG header: the 8-byte signature plus a
// well-formed IHDR chunk carrying the given dimensions. pngToICO reads only bytes
// 16..23, so no pixel data or valid CRC is required. These are entirely
// fabricated bytes, never a real asset.
func synthPNG(width, height uint32) []byte {
	p := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A} // PNG signature
	p = append(p, 0, 0, 0, 13)                               // IHDR chunk length
	p = append(p, 'I', 'H', 'D', 'R')                        // IHDR chunk type
	p = binary.BigEndian.AppendUint32(p, width)              // width  (offset 16)
	p = binary.BigEndian.AppendUint32(p, height)             // height (offset 20)
	p = append(p, 8, 6, 0, 0, 0)                             // bit depth, color type, compression, filter, interlace
	p = append(p, 0xDE, 0xAD, 0xBE, 0xEF)                    // fabricated CRC
	return p
}

func TestPngToICO(t *testing.T) {
	png := synthPNG(64, 64)
	ico := pngToICO(png)

	const headerSize = 22 // 6-byte ICONDIR + 16-byte ICONDIRENTRY
	if len(ico) != headerSize+len(png) {
		t.Fatalf("ICO length = %d, want %d", len(ico), headerSize+len(png))
	}

	// ICONDIR: reserved=0, image type=1 (icon), image count=1.
	if reserved := binary.LittleEndian.Uint16(ico[0:2]); reserved != 0 {
		t.Errorf("reserved = %d, want 0", reserved)
	}
	if typ := binary.LittleEndian.Uint16(ico[2:4]); typ != 1 {
		t.Errorf("image type = %d, want 1", typ)
	}
	if count := binary.LittleEndian.Uint16(ico[4:6]); count != 1 {
		t.Errorf("image count = %d, want 1", count)
	}

	// ICONDIRENTRY width/height mirror the IHDR dimensions.
	if ico[6] != 64 {
		t.Errorf("entry width = %d, want 64", ico[6])
	}
	if ico[7] != 64 {
		t.Errorf("entry height = %d, want 64", ico[7])
	}

	// Blob size and offset must locate the PNG exactly.
	if size := binary.LittleEndian.Uint32(ico[14:18]); size != uint32(len(png)) {
		t.Errorf("bytes in resource = %d, want %d", size, len(png))
	}
	if offset := binary.LittleEndian.Uint32(ico[18:22]); offset != headerSize {
		t.Errorf("image offset = %d, want %d", offset, headerSize)
	}
	if !bytes.Equal(ico[headerSize:], png) {
		t.Error("PNG payload was not appended verbatim after the header")
	}
}

// A 256x256 image cannot fit in the single-byte width/height fields; the ICO
// format encodes 256 as the 0 sentinel.
func TestPngToICODimension256Sentinel(t *testing.T) {
	ico := pngToICO(synthPNG(256, 256))
	if ico[6] != 0 {
		t.Errorf("entry width for 256px = %d, want 0 (sentinel)", ico[6])
	}
	if ico[7] != 0 {
		t.Errorf("entry height for 256px = %d, want 0 (sentinel)", ico[7])
	}
}
