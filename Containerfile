# Build on a machine with internet access. The result needs nothing from the
# network at run time: the UI, its fonts and icons, and the documentation of
# Envoy Gateway, Envoy AI Gateway, KServe, vLLM and llm-d are all inside the
# image.
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
COPY clients/ clients/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /aigw-ui ./cmd/server

# The documentation of the projects the platform is built on, at the
# versions docs/upstream/projects.json pins: the pages as Markdown and the
# images they show (cmd/upstream-docs). It changes only with that file, so
# the build cache keeps it between builds.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27 AS upstream
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/upstream-docs/ cmd/upstream-docs/
COPY docs/upstream/projects.json docs/upstream/projects.json
RUN go run ./cmd/upstream-docs fetch -projects docs/upstream/projects.json -out /upstream-docs

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
# git (and ssh for an SSH remote): the Deployments page commits the models'
# values files to a git repository, which Argo CD deploys.
RUN microdnf install -y --nodocs git-core openssh-clients && microdnf clean all
LABEL name="aigw-ui" \
      summary="UI and API that manage Envoy AI Gateway on several clusters from one hub" \
      io.k8s.display-name="AI Gateway Control" \
      io.openshift.expose-services="8080:http"
COPY --from=server /aigw-ui /usr/local/bin/aigw-ui
COPY --from=web /src/web/dist /opt/aigw-ui/web
COPY --from=upstream /upstream-docs /opt/aigw-ui/upstream-docs
ENV UI_DIR=/opt/aigw-ui/web \
    UPSTREAM_DOCS_DIR=/opt/aigw-ui/upstream-docs \
    LISTEN_ADDR=:8080
EXPOSE 8080
# OpenShift replaces this with a random UID. The image writes nothing to its
# own disk; the clone of the deployments repository is on a volume
# (GITOPS_WORK_DIR).
USER 1001
ENTRYPOINT ["/usr/local/bin/aigw-ui"]
