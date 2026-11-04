FROM golang:1.27.3-trixie AS build

ARG VERSION

WORKDIR /go/src/sunet-vcl-validator
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/sunet-vcl-validator
RUN CGO_ENABLED=0 go test -c -o /go/bin/sunet-vcl-validator.test

# The vinyl image is only pinned here so the tests below always run against
# the same vinyld that is shipped.
FROM platform.sunet.se/sunet-cdn/cdn-vinyl@sha256:910c3ffed7d883e5fc9a4e6f53db05fda99e28534f8c0abfe9e289b99e69ff0f AS vinyl

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
