# syntax=docker/dockerfile:1
# Multi-arch (linux/amd64, linux/arm64):
#   docker buildx build --platform linux/amd64,linux/arm64 -f deploy/docker/Dockerfile -t openvms-recorder .
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o /out/openvms-recorder ./cmd/recorder

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home /data recorder \
 && install -d -o recorder -g recorder /data /recordings
COPY --from=build /out/openvms-recorder /usr/local/bin/openvms-recorder
USER recorder
ENV OPENVMS_RECORDER_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8640
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/bin/env", "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/8640 && printf 'GET /health/ready HTTP/1.0\\r\\n\\r\\n' >&3 && grep -q '200 OK' <&3"]
ENTRYPOINT ["/usr/local/bin/openvms-recorder"]
CMD ["serve", "-config", "/config/recorder.yaml"]
