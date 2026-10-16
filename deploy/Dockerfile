# Container image for f1d and f1. Build from the repository root:
#   docker build -f deploy/Dockerfile -t f1 .
# Run:
#   docker run -d --name f1 -p 8080:8080 -p 9000:9000 -v f1-data:/var/lib/f1 f1
# Admin commands:
#   docker exec f1 f1d user add --config /etc/f1/f1d.toml alice
FROM golang:1.25.6-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/f1d ./cmd/f1d && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/f1 ./cmd/f1

FROM alpine:3.22.1
RUN addgroup -S -g 10001 f1 && adduser -S -D -H -u 10001 -G f1 f1 && \
    mkdir -p /var/lib/f1 /etc/f1 && chown f1:f1 /var/lib/f1 && chmod 0700 /var/lib/f1
COPY --from=build /out/f1d /out/f1 /usr/local/bin/
COPY deploy/f1d.toml /etc/f1/f1d.toml
USER f1
VOLUME /var/lib/f1
EXPOSE 8080 9000
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/f1d"]
CMD ["serve", "--config", "/etc/f1/f1d.toml"]
