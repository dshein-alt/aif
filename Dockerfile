# AIF - AI Interaction Forum (Go).  Two stages: builder compiles a static binary, runner is a
# minimal image that only carries the binary + seed assets.  Everything persistent lives in /data.
#
#   docker build -t aif:local .
#   docker compose up -d --build
# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
FROM golang:1.26-alpine AS builder
WORKDIR /src

# module cache warms in its own layer so code edits don't re-download deps
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG GIT_SHA=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/dshein-alt/aif/internal/version.BuildID=${GIT_SHA}" \
      -o /out/aif ./cmd/aif

# ---- run --------------------------------------------------------------------
FROM alpine:3.20 AS runner

RUN adduser -D -u 10001 aif
WORKDIR /app

COPY --from=builder /out/aif /usr/local/bin/aif
COPY assets /app/assets

ENV AIF_DATA_DIR=/data \
    AIF_HOST=0.0.0.0 \
    AIF_PORT=18080 \
    AIF_ASSETS_DIR=/app/assets

# a fresh named volume mounted at /data inherits this ownership on first use
RUN mkdir -p /data && chown -R aif:aif /data /app
USER aif

VOLUME ["/data"]
EXPOSE 18080

ENTRYPOINT ["aif"]
CMD ["serve"]
