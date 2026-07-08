// Command mkico packs one or more PNG images into a single multi-entry ICO
// container, used at build time to embed the app icon as a Windows PE resource.
//
// Windows Vista and later accept PNG-compressed images stored directly inside an
// ICO, so each PNG is embedded verbatim — no decoding or re-encoding. The output
// is a 6-byte ICONDIR, one 16-byte ICONDIRENTRY per image, then the PNG blobs
// appended in order, with each entry's offset chained past the ones before it.
//
// Usage:
//
//	go run ./tools/mkico -o out.ico small.png medium.png large.png
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

func main() {
	out := flag.String("o", "", "path to write the ICO to")
	flag.Parse()

	if *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: mkico -o out.ico in1.png [in2.png ...]")
		os.Exit(2)
	}

	pngs := make([][]byte, 0, flag.NArg())
	for _, path := range flag.Args() {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mkico: %v\n", err)
			os.Exit(1)
		}
		pngs = append(pngs, b)
	}

	ico, err := buildICO(pngs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkico: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(*out, ico, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "mkico: %v\n", err)
		os.Exit(1)
	}
}

// buildICO packs the given PNG images, verbatim, into a multi-entry ICO. The
// entries appear in the same order as the input; each carries the image's pixel
// dimensions (read from its PNG header) and a byte offset chained past every
// blob before it.
func buildICO(pngs [][]byte) ([]byte, error) {
	if len(pngs) == 0 {
		return nil, fmt.Errorf("no input images")
	}

	// ICONDIR (6 bytes) + one ICONDIRENTRY (16 bytes) per image; the first blob
	// starts immediately after this fixed-size directory.
	const dirEntrySize = 16
	headerSize := 6 + dirEntrySize*len(pngs)

	dir := make([]byte, 0, headerSize)
	// ICONDIR: reserved=0, image type=1 (icon), image count=N (each uint16 LE).
	dir = append(dir, 0, 0, 1, 0)
	dir = binary.LittleEndian.AppendUint16(dir, uint16(len(pngs)))

	blobs := make([]byte, 0)
	offset := headerSize
	for _, png := range pngs {
		w, h := pngDims(png)
		// ICONDIRENTRY.
		dir = append(dir,
			w, // width  (0 => 256)
			h, // height (0 => 256)
			0, // palette color count (0 => no palette)
			0, // reserved
		)
		dir = binary.LittleEndian.AppendUint16(dir, 1)                // color planes
		dir = binary.LittleEndian.AppendUint16(dir, 32)               // bits per pixel
		dir = binary.LittleEndian.AppendUint32(dir, uint32(len(png))) // bytes in resource
		dir = binary.LittleEndian.AppendUint32(dir, uint32(offset))   // offset to the blob

		offset += len(png)
		blobs = append(blobs, png...)
	}

	return append(dir, blobs...), nil
}

// pngDims reads the pixel dimensions from a PNG's IHDR (width at byte offset 16,
// height at 20, each a big-endian uint32) and folds any value of 256 or larger
// down to the 0 sentinel that the single-byte ICO width/height fields use.
func pngDims(png []byte) (w, h byte) {
	if len(png) >= 24 {
		if width := binary.BigEndian.Uint32(png[16:20]); width < 256 {
			w = byte(width)
		}
		if height := binary.BigEndian.Uint32(png[20:24]); height < 256 {
			h = byte(height)
		}
	}
	return w, h
}
