package localauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Known browsers: a browser that signed in as the owner keeps a token
// (a cookie) saying when, MAC'd with a key kept in the database. It only
// exempts that browser from the owner-wide password delay (see
// Throttle); `publisher reset-login` makes a new key, forgetting every
// browser.
const (
	DeviceTTL     = 400 * 24 * time.Hour // the longest a browser keeps a cookie
	deviceKeyName = "device_key"
)

func deviceMAC(key []byte, issued string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("publisher device " + issued))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// DeviceToken is a new known-browser token.
func (s *Service) DeviceToken(ctx context.Context) (string, error) {
	key, err := s.Store.InstanceKey(ctx, deviceKeyName)
	if err != nil {
		return "", err
	}
	issued := strconv.FormatInt(time.Now().Unix(), 10)
	return issued + "." + deviceMAC(key, issued), nil
}

// KnownDevice reports whether tok is a live known-browser token.
func (s *Service) KnownDevice(ctx context.Context, tok string) bool {
	issued, mac, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 128 {
		return false
	}
	at, err := strconv.ParseInt(issued, 10, 64)
	if err != nil || time.Since(time.Unix(at, 0)) > DeviceTTL {
		return false
	}
	key, err := s.Store.InstanceKey(ctx, deviceKeyName)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(deviceMAC(key, issued)))
}
