# This Dockerfile was generated from github.com/sourcegraph/godockerize. It
# was not written by a human, and as such looks janky. As you change this
# file, please don't be scared to make it more pleasant / remove hadolint
# ignores.

FROM sourcegraph/alpine-3.14:166590_2022-08-11_7ebaa5ea4d88@sha256:f6b878c33efb48a151f112a996f3f71b59e3052288cade537bc6b538f0a2450e

ARG COMMIT_SHA="unknown"
ARG DATE="unknown"
ARG VERSION="unknown"

LABEL org.opencontainers.image.revision=${COMMIT_SHA}
LABEL org.opencontainers.image.created=${DATE}
LABEL org.opencontainers.image.version=${VERSION}
LABEL com.sourcegraph.github.url=https://github.com/sourcegraph/sourcegraph/commit/${COMMIT_SHA}

ENV LOG_REQUEST=true
USER sourcegraph
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/github-proxy"]
COPY github-proxy /usr/local/bin/
