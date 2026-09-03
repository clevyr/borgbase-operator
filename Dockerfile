# Build the manager binary.
#
# The builder runs on the platform doing the build and cross-compiles via
# GOARCH. Pinning it to the target platform instead makes buildx run the entire
# Go toolchain under QEMU for any foreign architecture, which turns a one minute
# arm64 build into a fifteen minute one. Nothing in the final stage executes, so
# no emulation is needed anywhere.
FROM --platform=$BUILDPLATFORM golang:1.27.1 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go module manifests first so that source changes do not invalidate
# the downloaded dependency layer.
COPY go.mod go.mod
COPY go.sum go.sum
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# No -a: rebuilding every dependency including the standard library on every
# build defeats the compiler cache for no benefit.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
