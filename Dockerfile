# syntax=docker/dockerfile:1.7

# ---- build ----
# go-nostr v0.52.3 requires Go >= 1.24.1
# golang.org/x/image v0.40.0 requires Go >= 1.25.0
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache module downloads as a separate layer — only re-fetches when go.mod or
# go.sum change. Source edits below this don't invalidate the dep cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Trim path + strip symbol table → smallest static binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags='-s -w' \
      -o /publisher \
      ./cmd/publisher

# ---- runtime ----
FROM scratch

# CA bundle (for outbound TLS to blossom + relays over wss://).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Static binary.
COPY --from=build /publisher /publisher

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/publisher"]
