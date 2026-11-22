# syntax=docker/dockerfile:1

# The build stage runs on the build host's platform and cross-compiles for the
# target platform, so multi-arch builds don't need emulation for compiling.
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/greylistd-frontend ./cmd/greylistd-frontend

FROM scratch
# CA certificates for talking to the OpenID Connect provider over TLS. If your
# provider uses a private CA, mount its bundle over this file or set
# SSL_CERT_FILE to a mounted bundle.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/greylistd-frontend /greylistd-frontend
# greylistd's socket is mode 0660, so the container must run with the uid/gid
# that owns it (e.g. `docker run --user 108:108`). Default to nobody.
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/greylistd-frontend"]
