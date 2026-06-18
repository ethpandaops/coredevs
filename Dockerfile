FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o coredevs ./cmd/coredevs

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 coredevs

WORKDIR /app

COPY --from=builder /app/coredevs /app/coredevs
COPY config.yaml /app/config.yaml

USER coredevs

EXPOSE 8080

ENTRYPOINT ["/app/coredevs"]
CMD ["--config=/app/config.yaml"]
