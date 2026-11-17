# godepot as a static program in an image with nothing else in it: no shell, no package manager, a user that is not root, and nothing to write (godepot keeps no state on a disk).
# The program is built on the machine that builds the image, for the machine the image is for (no emulation): `docker buildx build --platform linux/amd64,linux/arm64`.
# renovate: datasource=docker depName=golang
ARG GO_VERSION=1.26.11
# renovate: datasource=docker depName=gcr.io/distroless/static
ARG RUNTIME_TAG=nonroot

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/godepot ./cmd/godepot

FROM gcr.io/distroless/static:${RUNTIME_TAG}
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="godepot" \
      org.opencontainers.image.description="A repository manager on top of blobgate: bytes kept once by digest, collected while it serves" \
      org.opencontainers.image.source="https://github.com/zystem/godepot" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /out/godepot /godepot
USER 65532:65532
EXPOSE 18100
ENTRYPOINT ["/godepot"]
