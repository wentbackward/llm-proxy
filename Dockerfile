FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o llm-proxy ./cmd/llm-proxy

FROM scratch
COPY --from=builder /app/llm-proxy /llm-proxy
ENTRYPOINT ["/llm-proxy"]
