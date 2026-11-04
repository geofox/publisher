# syntax=docker/dockerfile:1.7

# ---- build ----
# go-nostr v0.52.3 requires Go >= 1.24.1
# golang.org/x/image v0.40.0 requires Go >= 1.25.0
# bluesky-social/indigo (atproto SDK) requires Go >= 1.26
#
# Multi-arch (linux/amd64, linux/arm64): the build stage always runs on the
# builder's own platform and cross-compiles for the target (pure Go,
# CGO_ENABLED=0), so no QEMU is needed for the Go build.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

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
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build \
      -trimpath \
      -tags nodynamic \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /publisher \
      ./cmd/publisher

# Donor for the runtime /tmp (scratch has no filesystem; streamed video
# uploads + transcode outputs land there — the overlay layer is disk-backed).
RUN mkdir -p /newtmp && chmod 1777 /newtmp

# ---- static ffmpeg/ffprobe for the target platform ----
# Pinned by the multi-arch INDEX digest of mwader/static-ffmpeg:7.1.1 (amd64 +
# arm64), so each target platform resolves its own binaries while the pin
# stays immutable. (The previous pin, sha256:6769881c…, was the amd64 child
# manifest of this same index.)
FROM --platform=$TARGETPLATFORM mwader/static-ffmpeg:7.1.1@sha256:11a44711684c0b9f754c047dcd64235b8b52deab251bd0e0a86f22faa160749c AS ffmpeg

# ---- linkage + architecture guard ----
# A dynamically linked binary in FROM scratch dies at exec with a misleading
# "no such file or directory" (the missing file is the ELF interpreter) — it
# crash-looped v1.8.0 in production. Fail the BUILD instead, for every binary
# that ships: a dynamic loader path embedded in the file means dynamic linkage
# (the pattern also matches the aarch64 loaders).
#
# Each binary's ELF e_machine (2 bytes at offset 18, little-endian) must also
# match the target: a wrong-arch ffmpeg would otherwise only fail at the first
# video upload. 3e00 = x86-64 (62), b700 = AArch64 (183).
FROM build AS check
ARG TARGETARCH
COPY --from=ffmpeg /ffmpeg /ffprobe /check/
RUN cp /publisher /check/publisher && \
    case "$TARGETARCH" in \
      amd64) want=3e00 ;; \
      arm64) want=b700 ;; \
      *) echo "FATAL: unsupported TARGETARCH $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    for b in /check/publisher /check/ffmpeg /check/ffprobe; do \
      if grep -qaE 'ld-(linux|musl)' "$b"; then \
        echo "FATAL: $b is dynamically linked — scratch cannot exec it" >&2; exit 1; \
      fi; \
      got=$(od -An -tx1 -j18 -N2 "$b" | tr -d ' \n'); \
      if [ "$got" != "$want" ]; then \
        echo "FATAL: $b is ELF machine $got, want $want for $TARGETARCH" >&2; exit 1; \
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

# Readiness via /readyz (database + disk; never upstreams). Compose/k8s probes
# can override this; `docker ps` shows health without any extra config.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/publisher", "-healthcheck"]

ENTRYPOINT ["/publisher"]
