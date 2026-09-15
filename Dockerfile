# ---- Builder stage ----
FROM golang:1.25-alpine AS builder

WORKDIR /src

# 先拷贝依赖清单，利用 Docker 层缓存，避免源码变更时重复下载依赖
COPY go.mod go.sum ./
RUN go mod download

# 拷贝源码并编译（modernc.org/sqlite 为纯 Go 实现，无需 CGO）
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /starburst-backend ./cmd/starburst-backend

# ---- Runtime stage ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 appuser \
    && mkdir -p /data \
    && chown appuser:appuser /data

COPY --from=builder /starburst-backend /usr/local/bin/starburst-backend

USER appuser

EXPOSE 8080
VOLUME ["/data"]

ENV OCB_LISTEN=:8080 \
    OCB_SQLITE_PATH=/data/starburst-backend.db

ENTRYPOINT ["starburst-backend"]