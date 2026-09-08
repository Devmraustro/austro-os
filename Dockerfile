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

RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

COPY --from=builder /app/main .
COPY --from=builder /app/worker .

USER appuser

EXPOSE 8080

CMD ["./main"]
