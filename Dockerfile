# Stage 1 (Build)
# Run the builder natively on the build host and let Go cross-compile for the
# target platform, instead of emulating the whole Go toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS builder

RUN apk add --update --no-cache git make
WORKDIR /app/
COPY go.mod go.sum /app/
RUN go mod download
COPY . /app/
# Declare build args only where they're used: every RUN after an ARG gets it in
# its environment, so declaring VERSION (dev-<sha>) earlier would invalidate the
# cache for apk add / go mod download on every commit, and TARGETARCH would make
# those layers differ per platform.
ARG VERSION
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -ldflags="-s -w -X github.com/pelican/wings/system.Version=$VERSION" \
    -v \
    -trimpath \
    -o wings \
    wings.go
RUN echo "ID=\"distroless\"" > /etc/os-release

# Stage 2 (Final)
FROM gcr.io/distroless/static:latest
COPY --from=builder /etc/os-release /etc/os-release

COPY --from=builder /app/wings /usr/bin/
CMD [ "/usr/bin/wings", "--config", "/etc/pelican/config.yml" ]

EXPOSE 80 8080 2022
