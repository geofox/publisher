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

# Regression guard: a dynamically linked binary embeds its interpreter path
# (ld-linux/ld-musl); fail the image build here rather than crash-loop on the
# host. grep -a treats the ELF as text; static binaries contain neither string.
RUN if grep -qaE 'ld-(linux|musl)' /publisher; then \
      echo "FATAL: /publisher is dynamically linked — scratch cannot exec it" >&2; \
      exit 1; \
    fi

# ---- runtime ----
FROM scratch

# CA bundle (for outbound TLS to blossom + relays over wss://).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Static binary.
COPY --from=build /publisher /publisher

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/publisher"]
