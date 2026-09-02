FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

ENV GOTOOLCHAIN=local
ENV GOFLAGS=-mod=mod

WORKDIR /app

COPY go.mod ./
COPY *.go ./
COPY internal ./internal
COPY infrastructure ./infrastructure
COPY cmd ./cmd

RUN go mod tidy && go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o main .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o worker ./cmd/worker/main.go

FROM alpine:3.19

RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

COPY --from=builder /app/main .
COPY --from=builder /app/worker .

USER appuser

EXPOSE 8080

CMD ["./main"]
