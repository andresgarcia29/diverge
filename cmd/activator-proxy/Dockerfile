# golang:1.26.10
FROM golang@sha256:670bb5aa5349ae03b8516764718486f4f16399134bdc9acd0c46a23f043ab05c AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /activator-proxy ./cmd/activator-proxy/

# gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
COPY --from=builder /activator-proxy /activator-proxy
USER 65532:65532
ENTRYPOINT ["/activator-proxy"]
