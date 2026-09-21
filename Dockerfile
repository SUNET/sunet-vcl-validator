FROM golang:1.26.3-trixie AS build

ARG VERSION

WORKDIR /go/src/sunet-vcl-validator
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/sunet-vcl-validator

FROM platform.sunet.se/sunet-cdn/cdn-vinyl@sha256:d7993cd531fc0f194ce1f1f480b6e3b15647931d7e7161608e635317b2391134

# Temporarily change user to root to allow directory creation
USER root
RUN mkdir -p /shared/unix-sockets && chown -R vinyl:vinyl /shared
# Change back to user from the base image
USER vinyl

COPY --from=build /go/bin/sunet-vcl-validator /
ENTRYPOINT ["/sunet-vcl-validator"]
