# syntax=docker/dockerfile:1
# Build on the native BuildKit platform. The final scratch stage only copies
# static files, so target binaries never execute during assembly.
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -buildvcs=false -trimpath -o /out/derper ./cmd/derper

FROM scratch

COPY --from=build /out/derper /usr/local/bin/derper
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

ENV HOME=/var/lib/derper
WORKDIR /var/lib/derper

# DERP HTTPS, HTTP for ACME HTTP-01, and STUN respectively.
EXPOSE 443/tcp 80/tcp 3478/udp

ENTRYPOINT ["/usr/local/bin/derper"]
