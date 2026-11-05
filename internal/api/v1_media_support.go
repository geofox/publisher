package api

import (
	"log/slog"
	"os"
	"time"

	"github.com/geofox/publisher/internal/verify"
	"github.com/geofox/publisher/internal/videojob"
)

// sourceMediaClient is SSRF-guarded: source media URLs come from a pasted
// (untrusted) post, so a fetch must never reach internal/loopback addresses.
var sourceMediaClient = verify.NewSafeClient(20 * time.Second)

// videoWorkdir is the DEDICATED dir for uploads and transcode outputs — never
// bare os.TempDir(): the runner's startup sweep deletes everything in its
// workdir, and dev /tmp is RAM-backed tmpfs. Configurable via VIDEO_WORKDIR.
func (a *API) videoWorkdir() string {
	d := a.VideoWorkdir
	if d == "" {
		d = videojob.DefaultWorkdir()
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		slog.Error("video workdir unavailable", "path", d, "err", err)
	}
	return d
}

// compressSem bounds concurrent transcodes: each can transiently allocate
// hundreds of MB (decode + flatten + orientation copies of a ~100MP image),
// and this host has no swap. Two slots keep the composer snappy while making
// pile-ups impossible.
var compressSem = make(chan struct{}, 2)
