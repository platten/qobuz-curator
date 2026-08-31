# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.27.0 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN mkdir -p /out/data && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o /out/qobuz-curator .

FROM scratch
LABEL org.opencontainers.image.title="Qobuz Curator" \
      org.opencontainers.image.description="Local-first playlist recommendation and Qobuz publishing tool" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/qobuz-curator /qobuz-curator
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV QOBUZ_CURATOR_HOST=0.0.0.0 \
    QOBUZ_CURATOR_PORT=49277 \
    QOBUZ_CURATOR_DATA_DIR=/data \
    QOBUZ_CURATOR_LOG_FORMAT=json \
    QOBUZ_CURATOR_LOG_COLOR=false
EXPOSE 49277
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/qobuz-curator", "healthcheck"]
ENTRYPOINT ["/qobuz-curator"]
CMD ["serve", "--no-browser"]
