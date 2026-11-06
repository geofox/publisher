package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// qaOrientedGPSJPEG: a w x h JPEG whose APP1 EXIF carries orientation 6 plus a
// recognisable GPS marker, like a phone photo shot in portrait.
func qaOrientedGPSJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	plain := buf.Bytes()
	tiff := &bytes.Buffer{}
	tiff.WriteString("MM")
	binary.Write(tiff, binary.BigEndian, uint16(42))
	binary.Write(tiff, binary.BigEndian, uint32(8))
	binary.Write(tiff, binary.BigEndian, uint16(1))
	binary.Write(tiff, binary.BigEndian, uint16(0x0112))
	binary.Write(tiff, binary.BigEndian, uint16(3))
	binary.Write(tiff, binary.BigEndian, uint32(1))
	binary.Write(tiff, binary.BigEndian, uint16(6))
	binary.Write(tiff, binary.BigEndian, uint16(0))
	binary.Write(tiff, binary.BigEndian, uint32(0))
	tiff.WriteString("GPSLatitude=48.8584N GPSLongitude=2.2945E")
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	app1 := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	app1 = append(app1, payload...)
	out := append([]byte{0xFF, 0xD8}, app1...)
	return append(out, plain[2:]...)
}

func qaBlossom(t *testing.T, got *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://blossom.example/x"}`))
	}))
}

// Finding: the public Blossom canonical keeps the photo's EXIF/GPS.
func TestProcessUploadsGPSToBlossom(t *testing.T) {
	var got []byte
	srv := qaBlossom(t, &got)
	defer srv.Close()
	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))
	if _, err := p.Process(context.Background(), qaOrientedGPSJPEG(64, 32), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("GPSLatitude")) {
		t.Error("bytes PUT to Blossom (public, content-addressed) still contain the EXIF GPS payload")
	}
}

// Finding: dim (NIP-92 imeta, Bluesky/Threads aspect, media row) and blurhash
// are computed on the stored pixel grid, ignoring EXIF orientation.
func TestProcessDimIgnoresOrientation(t *testing.T) {
	var got []byte
	srv := qaBlossom(t, &got)
	defer srv.Close()
	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))
	res, err := p.Process(context.Background(), qaOrientedGPSJPEG(64, 32), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if res.Dim != "32x64" {
		t.Errorf("Dim = %q, want display dims 32x64 for EXIF orientation 6", res.Dim)
	}
}

// Fetch (the dispatch re-pull of videos up to FetchCap=110 MB) must not run
// on the small-object client's fixed overall timeout: a video that takes
// longer than it to pull from Blossom could never be fetched.
func TestFetchOutlastsTheSmallObjectTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(3<<20))
		chunk := make([]byte, 1<<20)
		for i := 0; i < 3; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(700 * time.Millisecond)
		}
	}))
	defer srv.Close()
	sk := nostr.Generate()
	p := New("http://unused", sk, nostr.GetPublicKey(sk))
	p.HTTP.Timeout = time.Second // stands in for the 30 s budget
	if _, _, err := p.Fetch(context.Background(), srv.URL+"/v.mp4"); err != nil {
		t.Errorf("a slow video fetch hit the small-object timeout: %v", err)
	}
}
