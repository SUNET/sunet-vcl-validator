FROM golang:1.25.7-trixie AS build

ARG VERSION

WORKDIR /go/src/sunet-vcl-validator
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/sunet-vcl-validator

FROM platform.sunet.se/sunet-cdn/cdn-varnish@sha256:2cf166b34db3cd87dd012183cf04c488fe834ef1575b450b069197ce2854440b

# Temporarily change user to root to allow directory creation
USER root
RUN mkdir -p /shared/unix-sockets && chown -R varnish:varnish /shared
# Change back to user from the base image
USER varnish

COPY --from=build /go/bin/sunet-vcl-validator /
ENTRYPOINT ["/sunet-vcl-validator"]
