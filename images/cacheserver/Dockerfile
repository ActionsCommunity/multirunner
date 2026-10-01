# Standalone multirunner cache server image (multi-arch).
# Build context = repo root:
#   docker buildx build -f images/cacheserver/Dockerfile --platform linux/amd64,linux/arm64 .
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /cacheserver ./cmd/cacheserver

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=build /cacheserver /cacheserver
VOLUME /data
EXPOSE 3000
ENTRYPOINT ["/cacheserver"]
