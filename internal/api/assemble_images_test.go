package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// multipartWithImages builds a parsed request carrying n "image" files.
func multipartWithImages(t *testing.T, n int) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for i := 0; i < n; i++ {
		fw, err := w.CreateFormFile("image", "i"+strconv.Itoa(i)+".png")
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte{0x89})
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestMaxImagesPerRequestConstant(t *testing.T) {
	if maxImagesPerRequest != 40 {
		t.Fatalf("maxImagesPerRequest=%d, want 40", maxImagesPerRequest)
	}
}

func TestAssembleImagesRejectsEleven(t *testing.T) {
	a := &API{}
	if _, _, err := a.assembleImages(multipartWithImages(t, 41), nil); err == nil || !strings.Contains(err.Error(), "max 40 images") {
		t.Fatalf("41 files must hit the cap, got %v", err)
	}
	if _, _, err := a.assembleImages(multipartWithImages(t, 0), make([]imageSpec, 41)); err == nil || !strings.Contains(err.Error(), "max 40 images") {
		t.Fatalf("41 specs must hit the cap, got %v", err)
	}
}

func TestAssembleImagesRejectsCombinedOverflow(t *testing.T) {
	// 21 fresh files + 21 Blossom references = 42 assembled images: Blossom-ref
	// specs don't consume files, and leftover files are processed by the
	// defensive trailing loop — the cap must bound the combined total.
	a := &API{}
	specs := make([]imageSpec, 21)
	for i := range specs {
		specs[i].BlossomURL = "https://blossom.example/" + strconv.Itoa(i)
	}
	if _, _, err := a.assembleImages(multipartWithImages(t, 21), specs); err == nil || !strings.Contains(err.Error(), "max 40 images") {
		t.Fatalf("21 files + 21 blossom refs must hit the cap, got %v", err)
	}
}
