# Engram in a container: the engine (ECP and MCP over HTTP), the trigger listener,
# the dashboard and the sleep cycle, sharing one database volume.
#   docker build -t engram .
#   docker compose up        (see docker-compose.yml)

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -trimpath \
      -ldflags "-s -w -X github.com/zakeripouya/EngramContextProtocol/internal/server.Version=${VERSION}" \
      -o /out/engram ./cmd/engram

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home engram \
    && mkdir -p /data && chown engram /data
COPY --from=build /out/engram /usr/local/bin/engram
USER engram
ENV ENGRAM_DB=/data/engram.db
VOLUME /data
EXPOSE 7079 7077 7078
HEALTHCHECK --interval=30s --timeout=3s CMD ["engram", "version"]
ENTRYPOINT ["engram"]
CMD ["serve", "--http", "0.0.0.0:7079"]
