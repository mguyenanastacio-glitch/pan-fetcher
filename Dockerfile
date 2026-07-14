# Stage 1: Build
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o pan-fetcher .

# Stage 2: Runtime
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata sqlite-libs

ENV TZ=Asia/Shanghai

WORKDIR /app
COPY --from=builder /build/pan-fetcher .
COPY --from=builder /build/config.example.toml ./config.toml
COPY --from=builder /build/indexers/ ./indexers/

VOLUME ["/app/data"]
EXPOSE 8115

ENTRYPOINT ["./pan-fetcher", "server"]
CMD ["--port", "8115"]
