# Stage 1 (Build)
# Run the builder natively on the build host and let Go cross-compile for the
# target platform, instead of emulating the whole Go toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS builder

ARG VERSION
ARG TARGETOS
ARG TARGETARCH
RUN apk add --update --no-cache git make
WORKDIR /app/
COPY go.mod go.sum /app/
RUN go mod download
COPY . /app/
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

EXPOSE 8080
