FROM golang:1.27.3-trixie AS build

ARG VERSION

WORKDIR /go/src/sunet-vcl-validator
COPY . .

RUN go mod download
# Run the tests with the race detector to check e.g. the socket reference
# counting. The race detector needs cgo, so this is done here rather than with
# the CGO_ENABLED=0 test binary below. The tests needing vinyld are skipped
# here and run in the test stage.
#
# Set GOARCH to match the pinned --platform below.
RUN go test -race ./...
RUN CGO_ENABLED=0 GOARCH=amd64 go build -o /go/bin/sunet-vcl-validator
RUN CGO_ENABLED=0 GOARCH=amd64 go test -c -o /go/bin/sunet-vcl-validator.test

# The vinyl image is only pinned here so the tests below always run against
# the same vinyld that is shipped. Pin --platform to make builds work on macOS
# since currently there are no linux/arm64 builds for cdn-vinyl. This causes a
# 'FromPlatformFlagConstDisallowed: FROM --platform flag should not use constant value "linux/amd64"'
# warning but let that be for now, the real solution would be to make cdn-vinyl
# have a native arm64 build.
FROM --platform=linux/amd64 platform.sunet.se/sunet-cdn/cdn-vinyl@sha256:ca98ef781c48f0c02cb37c5b89578caf959211c7a6d93d4ed1e37063c90e1b72 AS vinyl

# Temporarily change user to root to allow directory creation
USER root
RUN mkdir /haproxy-unix-sockets && chown -R vinyl:vinyl /haproxy-unix-sockets
# Change back to user from the base image
USER vinyl

# Run the tests against the real vinyld so a vinyl update that changes its
# behaviour (e.g. the wording of the "Backend path:" warnings or the C code
# setting backend paths, which we depend on) fails the build.
FROM vinyl AS test

COPY --from=build /go/bin/sunet-vcl-validator.test /tmp/
RUN SUNET_VCL_VALIDATOR_REQUIRE_VINYLD=1 /tmp/sunet-vcl-validator.test -test.v && touch /tmp/tests-passed

FROM vinyl

# BuildKit only builds stages the final image depends on, so copy a marker
# from the test stage to make sure the tests run and pass.
COPY --from=test /tmp/tests-passed /tmp/tests-passed
COPY --from=build /go/bin/sunet-vcl-validator /
ENTRYPOINT ["/sunet-vcl-validator"]
