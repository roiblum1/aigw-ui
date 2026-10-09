# Build on a machine with internet access. The result needs nothing from the
# network at run time: the UI, its fonts and icons are all inside the image.
#
# The build stages run on the build machine's own architecture and
# cross-compile, so building for linux/amd64 on an arm64 laptop is fast.

FROM --platform=$BUILDPLATFORM docker.io/library/node:26 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27 AS server
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY docs/ docs/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /aigw-ui ./cmd/server

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
LABEL name="aigw-ui" \
      summary="UI and API that manage Envoy AI Gateway on several clusters from one hub" \
      io.k8s.display-name="AI Gateway Control" \
      io.openshift.expose-services="8080:http"
COPY --from=server /aigw-ui /usr/local/bin/aigw-ui
COPY --from=web /src/web/dist /opt/aigw-ui/web
ENV UI_DIR=/opt/aigw-ui/web \
    LISTEN_ADDR=:8080
EXPOSE 8080
# OpenShift replaces this with a random UID; the image writes nothing to disk.
USER 1001
ENTRYPOINT ["/usr/local/bin/aigw-ui"]
