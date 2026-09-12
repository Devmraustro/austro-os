FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

ENV GOTOOLCHAIN=local
ENV GOFLAGS=-mod=mod

WORKDIR /app

COPY go.mod go.sum ./
COPY *.go ./
COPY internal ./internal
COPY infrastructure ./infrastructure
COPY cmd ./cmd

RUN for i in $(seq 1 10); do go mod download && exit 0 || sleep 2; done; exit 1
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o main .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o worker ./cmd/worker

FROM alpine:3.19

# ca-certificates is required, not cosmetic: the AI gateway and the generic
# HTTP publisher make outbound HTTPS calls, and the Alpine base image ships no
# root store, so every such call fails certificate verification.
RUN apk add --no-cache ca-certificates \
    && addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

COPY --from=builder /app/main .
COPY --from=builder /app/worker .

USER appuser

EXPOSE 8080

# Liveness probe against the endpoint the orchestrator and the test suite use.
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/health/live || exit 1

CMD ["./main"]
