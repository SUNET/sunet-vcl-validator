FROM golang:1.25.7-trixie AS build

ARG VERSION

WORKDIR /go/src/sunet-vcl-validator
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/sunet-vcl-validator

FROM platform.sunet.se/sunet-cdn/cdn-varnish@sha256:3c67cd9f6368b98ec77a349c62ba6e589750e686f6e1f72da43ab7bcb064df92

# Temporarily change user to root to allow directory creation
USER root
RUN mkdir -p /shared/unix-sockets && chown -R varnish:varnish /shared
# Change back to user from the base image
USER varnish

COPY --from=build /go/bin/sunet-vcl-validator /
ENTRYPOINT ["/sunet-vcl-validator"]
