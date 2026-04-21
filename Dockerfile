FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bot .

FROM alpine:3.20
RUN adduser -D -g '' appuser
USER appuser
WORKDIR /app
COPY --from=builder /bot /app/bot

ENTRYPOINT ["/app/bot"]
