# Copyright (c) Privasys. All rights reserved.
# Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

# Stage 1: Build the Go API wrapper
FROM golang:1.23-bookworm AS builder
WORKDIR /build
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /lightpanda-api .

# Stage 2: Final image with Lightpanda + API wrapper
FROM lightpanda/browser:nightly AS lightpanda

FROM debian:stable-slim

RUN apt-get update -yq && \
    apt-get install -yq --no-install-recommends tini ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Copy Lightpanda browser binary
COPY --from=lightpanda /bin/lightpanda /bin/lightpanda

# Copy the Go API wrapper
COPY --from=builder /lightpanda-api /bin/lightpanda-api

EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/bin/lightpanda-api"]
