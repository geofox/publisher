package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
)

// craftPNGHeader builds a minimal valid PNG (signature + IHDR + IEND) declaring
// the given dimensions. DecodeConfig reads only the IHDR, so this exercises the
// pixel-bomb guard without ever allocating the (impossibly large) bitmap.
func craftPNGHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // color type: truecolor RGB
	writePNGChunk(&b, "IHDR", ihdr)
	writePNGChunk(&b, "IEND", nil)
	return b.Bytes()
}

func writePNGChunk(b *bytes.Buffer, typ string, data []byte) {
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

func TestExtractImageMetaRejectsPixelBomb(t *testing.T) {
	bomb := craftPNGHeader(20000, 20000) // 400 MP, over the 100 MP cap
	_, _, err := extractImageMeta(bomb)
	if err == nil {
		t.Fatal("expected the pixel-bomb header to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "pixel") {
		t.Fatalf("expected pixel-cap error, got %q", err.Error())
	}
}
