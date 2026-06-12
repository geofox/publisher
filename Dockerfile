# syntax=docker/dockerfile:1.7

# ---- build ----
# go-nostr v0.52.3 requires Go >= 1.24.1
# golang.org/x/image v0.40.0 requires Go >= 1.25.0
# bluesky-social/indigo (atproto SDK) requires Go >= 1.26
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache module downloads as a separate layer — only re-fetches when go.mod or
# go.sum change. Source edits below this don't invalidate the dep cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version metadata stamped into the binary for the publisher_build_info metric.
# Defaults keep local `docker build` working; CI passes the real tag + sha.
ARG VERSION=dev
ARG COMMIT=none

# Trim path + strip symbol table → smallest static binary. `nodynamic` compiles
# out gen2brain/heic's dynamic-library path (ebitengine/purego): purego's
# cgo_import_dynamic pragmas would otherwise make the binary dynamically linked
# even with CGO_ENABLED=0 — and FROM scratch has no ld-linux interpreter, so the
# container dies with "exec /publisher: no such file or directory" (v1.8.0).
# HEIC decoding stays fully functional via the WASM path (which the code forces
# at runtime anyway with heic.ForceWasmMode).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -tags nodynamic \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /publisher \
      ./cmd/publisher

# Donor for the runtime /tmp (scratch has no filesystem; streamed video
# uploads + transcode outputs land there — the overlay layer is disk-backed).
RUN mkdir -p /newtmp && chmod 1777 /newtmp

# ---- linkage guard ----
# A dynamically linked binary in FROM scratch dies at exec with a misleading
# "no such file or directory" (the missing file is the ELF interpreter) — it
# crash-looped v1.8.0 in production. Fail the BUILD instead, for every binary
# that ships: a dynamic loader path embedded in the file means dynamic linkage.
FROM build AS check
COPY --from=mwader/static-ffmpeg:7.1.1@sha256:6769881cc02c80d33e387750a8e144d162adfab2775e934dd97899261dda3a0c /ffmpeg /ffprobe /check/
RUN cp /publisher /check/publisher && \
    for b in /check/publisher /check/ffmpeg /check/ffprobe; do \
      if grep -qaE 'ld-(linux|musl)' "$b"; then \
        echo "FATAL: $b is dynamically linked — scratch cannot exec it" >&2; exit 1; \
      fi; \
    done && touch /check/ok

# ---- runtime ----
FROM scratch

# CA bundle (for outbound TLS to blossom + relays over wss://).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Static binary — copied from the check stage so the linkage guard is on the
# critical path.
COPY --from=check /check/publisher /publisher

# Static ffmpeg/ffprobe — also guard-gated via the check stage.
COPY --from=check /check/ffmpeg /check/ffprobe /usr/local/bin/

# Writable /tmp for streamed video uploads and transcode outputs.
COPY --from=build /newtmp /tmp

ENV PORT=8080
ENV FFMPEG_PATH=/usr/local/bin/ffmpeg
ENV FFPROBE_PATH=/usr/local/bin/ffprobe
EXPOSE 8080

ENTRYPOINT ["/publisher"]
