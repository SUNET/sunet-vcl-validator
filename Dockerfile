FROM golang:1.23.5-bookworm AS build

ARG VERSION

WORKDIR /go/src/sunet-vcl-validator
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/sunet-vcl-validator

FROM platform.sunet.se/sunet-cdn/cdn-varnish:af7f7d11e61acf9f6113811615d1baa46daf3bd1

# Temporarily change user to root to allow directory creation
USER root
RUN mkdir -p /shared/unix-sockets && chown -R varnish:varnish /shared
# Change back to user from the base image
USER varnish

COPY --from=build /go/bin/sunet-vcl-validator /
ENTRYPOINT ["/sunet-vcl-validator"]
