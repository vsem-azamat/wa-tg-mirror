# syntax=docker/dockerfile:1
FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/wa-tg-mirror ./cmd/wa-tg-mirror

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wa-tg-mirror /usr/local/bin/wa-tg-mirror
VOLUME /data
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/wa-tg-mirror"]
CMD ["serve", "-config", "/config/config.yaml", "-heartbeat", "/data/heartbeat"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=2m --retries=3 \
  CMD ["/usr/local/bin/wa-tg-mirror", "healthcheck", "/data/heartbeat"]
