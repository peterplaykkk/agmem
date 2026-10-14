# syntax=docker/dockerfile:1
#
# Self-contained multi-stage build:
#   web     — build the Vite/React SPA (web/dist), embedded into the server via go:embed
#   build   — static server binary (CGO_ENABLED=0, cmd/agmemserv)
#   runtime — minimal alpine image, non-root (uid 10001)
#
# The server image contains ONLY cmd/agmemserv. cmd/agmem (CLI) and cmd/agmemd
# (local cache daemon; cgo, onnxruntime loaded at runtime) are user-host tools
# and are built separately — see README "Build".
#
# GOARCH follows the build platform (TARGETARCH under BuildKit): no cross-image
# emulation. Build for another platform with `docker buildx build --platform`.
#
# Base image refs are overridable for air-gapped or mirrored registries:
#   docker build --build-arg GO_IMAGE=<registry>/golang:1.25-alpine .
ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.25-alpine
ARG RUNTIME_IMAGE=alpine:3.24

FROM ${NODE_IMAGE} AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web ./
RUN npm run build

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY scripts ./scripts
COPY web ./web
COPY --from=web /web/dist ./web/dist
ARG AGMEM_VERSION=dev
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${AGMEM_VERSION}" \
    -o /out/agmemserv ./cmd/agmemserv

FROM ${RUNTIME_IMAGE}
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/agmemserv /usr/local/bin/agmemserv
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/agmemserv"]
