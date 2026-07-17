# Stage 1: Build
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o pan-fetcher .

# Stage 2: Runtime
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata sqlite-libs curl

ENV TZ=Asia/Shanghai

WORKDIR /app
COPY --from=builder /build/pan-fetcher .
COPY --from=builder /build/indexers/ ./indexers/

# Default config — override via volume mount for production
RUN echo '[server]\nport = 8115' > /app/config.toml

VOLUME ["/app/data"]
EXPOSE 8115

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD curl -fs http://localhost:8115/ || exit 1

ENTRYPOINT ["./pan-fetcher", "server", "--port", "8115"]
