# syntax=docker/dockerfile:1

# Build for the target platform from the build machine's own Go toolchain;
# the binary is pure Go, so cross-compiling needs nothing else.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/hdhr-monitor ./cmd/hdhr-monitor
# The data directory, owned by the runtime user so a new volume is writable.
RUN mkdir -p /out/data

# Static base with CA certificates (for the channel guide service) and a
# non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hdhr-monitor /hdhr-monitor
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME /data
EXPOSE 8089
HEALTHCHECK --interval=30s --timeout=5s CMD ["/hdhr-monitor", "-healthcheck"]
ENTRYPOINT ["/hdhr-monitor"]
