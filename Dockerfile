FROM golang:1.27.1-trixie AS build

ARG VERSION

WORKDIR /go/src/sunet-vcl-validator
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/sunet-vcl-validator

FROM platform.sunet.se/sunet-cdn/cdn-vinyl@sha256:910c3ffed7d883e5fc9a4e6f53db05fda99e28534f8c0abfe9e289b99e69ff0f

# Temporarily change user to root to allow directory creation
USER root
RUN mkdir -p /shared/unix-sockets && chown -R vinyl:vinyl /shared
# Change back to user from the base image
USER vinyl

COPY --from=build /go/bin/sunet-vcl-validator /
ENTRYPOINT ["/sunet-vcl-validator"]
