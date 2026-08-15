# Sous is a single static binary plus embedded templates, so the runtime layer
# needs almost nothing. Build on the target arch via buildx; the CI workflow
# publishes linux/amd64 and linux/arm64 because gx10 is aarch64 while most
# development happens on x86.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first: they change far less often than the source, so this layer
# survives most rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off so the result is genuinely static and runs on a distroless-style
# base. -trimpath keeps build paths out of the binary; -s -w drops the symbol
# table and DWARF, which is a meaningful size cut for something with no
# debugger attached in production.
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/sous ./cmd/sous

# ---------------------------------------------------------------------------

FROM alpine:3.21

# ca-certificates: Sous fetches model metadata from HuggingFace over TLS.
# git: recipe sources are git mirrors, and Sous shells out to git for them.
# tzdata: deployment timestamps are rendered in local time.
RUN apk add --no-cache ca-certificates git tzdata

COPY --from=build /out/sous /usr/local/bin/sous

# Sous stores everything on disk deliberately - a broken Sous must be
# repairable with an editor - so this is a mount point, not a place to write
# into the image.
VOLUME ["/var/lib/sous"]

# No EXPOSE: the listen address is required configuration with no default,
# because binding everything on a component that can start and stop models
# would remove the only mitigation it has.

ENTRYPOINT ["/usr/local/bin/sous"]
