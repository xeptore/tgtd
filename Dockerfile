# syntax=docker/dockerfile:1
FROM docker.io/library/golang:1.23.2-alpine AS build
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

FROM python:3.13-alpine
RUN adduser -D nonroot
USER nonroot
COPY --chown=nonroot:nonroot --from=build /home/dev/src/bin/tgtd /home/nonroot/tgtd
WORKDIR /home/nonroot
RUN pip install --no-cache-dir --user tidal-dl
ENV TZ=UTC
ENV PATH="$PATH:/home/nonroot/.local/bin"
STOPSIGNAL SIGINT
ENTRYPOINT [ "/home/nonroot/tgtd" ]
