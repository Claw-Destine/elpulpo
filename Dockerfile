# syntax=docker/dockerfile:1
# Build the single static binary. Generated templ files are committed, so
# the build stage needs nothing but the Go toolchain.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/elpulpo ./cmd/elpulpo

# Pre-created, service-owned homes for the two mount points. Docker
# initialises a fresh named volume from the image directory, ownership
# included, so with these in place uid 65534 can open its SQLite database
# and save the YAML with no host-side chown. Without them the volume is
# root-owned and startup dies on the first write. The .keep markers exist
# because COPY does not carry empty directories.
RUN mkdir -p /staging/etc/elpulpo /staging/var/lib/elpulpo \
 && touch /staging/etc/elpulpo/.keep /staging/var/lib/elpulpo/.keep \
 && chown -R 65534:65534 /staging

# A shell-less image: CGO_ENABLED=0 makes it static, and the binary's
# --health flag replaces the shell the usual healthcheck would need.
FROM scratch
COPY --from=build /out/elpulpo /elpulpo
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --chown=65534:65534 --from=build /staging/etc/elpulpo/ /etc/elpulpo/
COPY --chown=65534:65534 --from=build /staging/var/lib/elpulpo/ /var/lib/elpulpo/

ENV ELPULPO_ADDR=:8080 \
    ELPULPO_CONFIG=/etc/elpulpo/elpulpo.yaml \
    ELPULPO_DATA_DIR=/var/lib/elpulpo \
    ELPULPO_LOG_LEVEL=info

EXPOSE 8080
VOLUME ["/etc/elpulpo", "/var/lib/elpulpo"]

USER 65534:65534
ENTRYPOINT ["/elpulpo"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD ["/elpulpo", "--health"]
