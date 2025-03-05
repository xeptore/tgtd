# syntax=docker/dockerfile:1
FROM docker.io/library/golang:1.24.1-alpine AS build
RUN <<eot
  set -Eeux
  apk update
  apk upgrade
  apk add --no-cache bash curl jq tar xz musl-dev linux-headers git
  sh -c "$(curl --location https://taskfile.dev/install.sh)" -- -d -b /usr/local/bin/
eot
RUN adduser -u 1001 -D dev
USER dev
WORKDIR /home/dev/src
COPY --chown=dev:dev . .
RUN task build

FROM docker.io/jrottenberg/ffmpeg:7.1-alpine
RUN adduser -D -u 1000 nonroot
USER nonroot
COPY --chown=nonroot:nonroot --from=build /home/dev/src/bin/tgtd /home/nonroot/tgtd
WORKDIR /home/nonroot
ENV TZ=UTC
STOPSIGNAL SIGINT
ENTRYPOINT [ "/home/nonroot/tgtd" ]
