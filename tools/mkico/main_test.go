package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// synthPNG builds a minimal synthetic PNG: the 8-byte signature, a well-formed
// IHDR carrying the given dimensions, and a short trailing marker so each blob is
// distinguishable byte-for-byte. buildICO reads only the header dimensions, so no
// pixel data or valid CRC is required. These are entirely fabricated bytes, never
// a real asset.
func synthPNG(width, height uint32, marker byte) []byte {
	p := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A} // PNG signature
	p = append(p, 0, 0, 0, 13)                               // IHDR chunk length
	p = append(p, 'I', 'H', 'D', 'R')                        // IHDR chunk type
	p = binary.BigEndian.AppendUint32(p, width)              // width  (offset 16)
	p = binary.BigEndian.AppendUint32(p, height)             // height (offset 20)
	p = append(p, 8, 6, 0, 0, 0)                             // bit depth, color type, compression, filter, interlace
	p = append(p, 0xDE, 0xAD, 0xBE, 0xEF)                    // fabricated CRC
	p = append(p, marker, marker, marker, marker)            // per-image marker
	return p
}

func TestBuildICOMultiEntry(t *testing.T) {
	pngs := [][]byte{
		synthPNG(16, 16, 0x11),
		synthPNG(48, 48, 0x22),
		synthPNG(256, 256, 0x33), // 256 must fold to the 0 sentinel
	}

	ico, err := buildICO(pngs)
	if err != nil {
		t.Fatalf("buildICO: %v", err)
	}

	const dirEntrySize = 16
	headerSize := 6 + dirEntrySize*len(pngs)

	total := headerSize
	for _, p := range pngs {
		total += len(p)
	}
	if len(ico) != total {
		t.Fatalf("ICO length = %d, want %d", len(ico), total)
	}

	// ICONDIR.
	if reserved := binary.LittleEndian.Uint16(ico[0:2]); reserved != 0 {
		t.Errorf("reserved = %d, want 0", reserved)
	}
	if typ := binary.LittleEndian.Uint16(ico[2:4]); typ != 1 {
		t.Errorf("image type = %d, want 1", typ)
	}
	if count := binary.LittleEndian.Uint16(ico[4:6]); int(count) != len(pngs) {
		t.Errorf("image count = %d, want %d", count, len(pngs))
	}

	wantDims := []struct{ w, h byte }{{16, 16}, {48, 48}, {0, 0}}
	offset := headerSize
	for i, png := range pngs {
		entry := ico[6+i*dirEntrySize : 6+(i+1)*dirEntrySize]

		if entry[0] != wantDims[i].w {
			t.Errorf("entry %d width = %d, want %d", i, entry[0], wantDims[i].w)
		}
		if entry[1] != wantDims[i].h {
			t.Errorf("entry %d height = %d, want %d", i, entry[1], wantDims[i].h)
		}
		if planes := binary.LittleEndian.Uint16(entry[4:6]); planes != 1 {
			t.Errorf("entry %d color planes = %d, want 1", i, planes)
		}
		if bpp := binary.LittleEndian.Uint16(entry[6:8]); bpp != 32 {
			t.Errorf("entry %d bits per pixel = %d, want 32", i, bpp)
		}
		if size := binary.LittleEndian.Uint32(entry[8:12]); int(size) != len(png) {
			t.Errorf("entry %d size = %d, want %d", i, size, len(png))
		}
		if off := binary.LittleEndian.Uint32(entry[12:16]); int(off) != offset {
			t.Errorf("entry %d offset = %d, want %d", i, off, offset)
		}

		// The blob at the declared offset must be the PNG appended verbatim.
		if got := ico[offset : offset+len(png)]; !bytes.Equal(got, png) {
			t.Errorf("entry %d blob was not appended verbatim", i)
		}
		offset += len(png)
	}
}

func TestBuildICOEmpty(t *testing.T) {
	if _, err := buildICO(nil); err == nil {
		t.Error("buildICO(nil) = nil error, want an error for no input images")
	}
}
