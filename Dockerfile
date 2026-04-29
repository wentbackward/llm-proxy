FROM golang:1.25-alpine AS builder
ARG VERSION=dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${VERSION}" -o hikyaku ./cmd/hikyaku
# Pre-create /capture so the default sig_message_capture.output_folder works
# when the container runs as a non-root UID. Owned by the runtime UID/GID.
RUN mkdir -p /rootfs/capture && chown -R 65532:65532 /rootfs

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/hikyaku /hikyaku
COPY --from=builder /rootfs/ /
USER 65532:65532
ENTRYPOINT ["/hikyaku"]
