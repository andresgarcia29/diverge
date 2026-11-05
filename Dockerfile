# Proto generation stage for web TypeScript types (bufbuild/buf:1.50.0)
FROM bufbuild/buf@sha256:c34c81ac26044490a10fb5009eb618640834b9048f38d4717538421c6a25e4d7 AS proto-gen
WORKDIR /workspace
COPY buf.yaml buf.gen.web.yaml ./
COPY api/ api/
RUN buf generate --template buf.gen.web.yaml

# Web dashboard build stage (node:22-alpine)
FROM node@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32 AS web-builder
WORKDIR /web
COPY web/package*.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
COPY --from=proto-gen /workspace/web/src/api/gen ./src/api/gen
RUN npm run build

# Go build stage (golang:1.26.10)
FROM golang@sha256:670bb5aa5349ae03b8516764718486f4f16399134bdc9acd0c46a23f043ab05c AS builder
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY gen/ gen/
COPY internal/ internal/
COPY pkg/ pkg/
COPY config/ config/

# Copy compiled web dashboard assets for go:embed
COPY --from=web-builder /web/dist internal/server/dashboard/dist/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -a -o /out/diverge-controller cmd/controller/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -a -o /out/diverge-proxy cmd/proxy/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -a -o /out/diverge-server ./cmd/server

# Runtime stage (gcr.io/distroless/static:nonroot)
FROM gcr.io/distroless/static@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
COPY --from=builder /out/diverge-controller /diverge-controller
COPY --from=builder /out/diverge-proxy /diverge-proxy
COPY --from=builder /out/diverge-server /diverge-server
USER 65532:65532
ENTRYPOINT ["/diverge-controller"]
