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

func TestAssembleImagesRejectsEleven(t *testing.T) {
	a := &API{}
	if _, _, err := a.assembleImages(multipartWithImages(t, 11), nil); err == nil || !strings.Contains(err.Error(), "max 10 images") {
		t.Fatalf("11 files must hit the cap, got %v", err)
	}
	if _, _, err := a.assembleImages(multipartWithImages(t, 0), make([]imageSpec, 11)); err == nil || !strings.Contains(err.Error(), "max 10 images") {
		t.Fatalf("11 specs must hit the cap, got %v", err)
	}
}
