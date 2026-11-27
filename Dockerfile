# Mirrors can be used through build args, e.g.
#   --build-arg BUILD_IMAGE=<mirror>/golang:1.26-bookworm --build-arg GOPROXY=<proxy>
ARG BUILD_IMAGE=golang:1.26-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

# Cross-compile on the build platform; only the runtime stage is per target.
FROM --platform=$BUILDPLATFORM ${BUILD_IMAGE} AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/helm-exporter .

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/helm-exporter /helm-exporter
USER 65532:65532
EXPOSE 9571
ENTRYPOINT ["/helm-exporter"]
