package bluesky

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/transcode"
)

// craftBigPNGHeader builds a valid PNG declaring huge dimensions in IHDR, padded
// past transcode.Bluesky.MaxBytes with an ancillary chunk. DecodeConfig reads
// only the IHDR (so it reports the bomb dimensions), while the >2 MB size forces
// fitBlob down the "can't safely resize" path — proving the guard fires before
// image.Decode allocates the bitmap.
func craftBigPNGHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8
	ihdr[9] = 2
	writeChunk(&b, "IHDR", ihdr)
	// tEXt padding to exceed transcode.Bluesky.MaxBytes (read after IHDR, so it
	// doesn't affect the dimensions DecodeConfig returns).
	pad := make([]byte, transcode.Bluesky.MaxBytes+1024)
	copy(pad, "Comment\x00")
	writeChunk(&b, "tEXt", pad)
	writeChunk(&b, "IEND", nil)
	return b.Bytes()
}

func writeChunk(b *bytes.Buffer, typ string, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	b.Write(length[:])
	crc := crc32.NewIEEE()
	b.WriteString(typ)
	crc.Write([]byte(typ))
	b.Write(data)
	crc.Write(data)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc.Sum32())
	b.Write(c[:])
}

func TestFitBlobRejectsPixelBomb(t *testing.T) {
	bomb := craftBigPNGHeader(20000, 20000) // 400 MP, over the 100 MP cap
	_, _, _, _, err := fitBlob(bomb, "image/png")
	if err == nil {
		t.Fatal("expected the oversized pixel-bomb to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "pixel") {
		t.Fatalf("expected pixel-cap error, got %q", err.Error())
	}
}
