# Copyright (c) Privasys. All rights reserved.
# Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

# Stage 1: Build the Go API wrapper
FROM golang:1.23-bookworm AS builder
WORKDIR /build
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /lightpanda-api .

# Stage 2: Final image with Lightpanda + API wrapper
# Lightpanda 0.3.2 (latest, 2026-06-16). Pinned by digest for reproducible builds
# instead of floating on a tag; bump the digest deliberately. (latest and nightly
# share this digest as of 2026-06-18.)
FROM lightpanda/browser:latest@sha256:f5e1bb6b11a7643796c02e89e3b6fb6c870be10d14dfee0db6d76b9afaf741dd AS lightpanda

FROM debian:stable-slim

RUN apt-get update -yq && \
    apt-get install -yq --no-install-recommends tini ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Copy Lightpanda browser binary
COPY --from=lightpanda /bin/lightpanda /bin/lightpanda

# Copy the Go API wrapper
COPY --from=builder /lightpanda-api /bin/lightpanda-api

# No EXPOSE: the app binds the platform-injected $PORT. Under host networking
# EXPOSE is a no-op anyway, and there is no fixed port to advertise.

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/bin/lightpanda-api"]
