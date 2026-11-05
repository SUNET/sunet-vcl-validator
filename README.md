# sunet-vcl-validator
This is the SUNET VCL validator server which allows you to POST a Vinyl VCL
file and have it validate the contents. Created as a supporting service to
https://github.com/SUNET/sunet-cdn-manager which needs a way to validate VCL
submitted to it prior to adding it to the database.

## Running
The service needs access to a `vinyld` binary for the actual validation so
running it as a container is probably the easiest.
```
docker build --platform=linux/amd64 -t sunet-vcl-validator:latest .
docker run -p 127.0.0.1:8888:8888 -it --rm sunet-vcl-validator:latest
```

## Usage
Given a VCL file you can submit it to the running service. A `200 OK` response
means the VCL file is OK, a `422 Unprocessable Entity` means it is invalid.

Example OK VCL:
```
$ curl -i --data-binary @vinyl.vcl 127.0.0.1:8888/validate-vcl
HTTP/1.1 200 OK
Request-Id: cuh2la0f9mos73cu60k0
Date: Tue, 04 Feb 2025 14:56:41 GMT
Content-Length: 0
```

Example invalid VCL:
```
$ curl -i --data-binary @vinyl-broken.vcl 127.0.0.1:8888/validate-vcl
HTTP/1.1 422 Unprocessable Entity
Content-Type: text/plain; charset=utf-8
Request-Id: cuh2lhof9mos73cu60kg
X-Content-Type-Options: nosniff
Date: Tue, 04 Feb 2025 14:57:11 GMT
Content-Length: 462

EEE </usr/lib/vinyl-cache/vmods/libvmod_slash.so>
eee </usr/lib/vinyl-cache/vmods/libvmod_slash.so>
ee2 vext_cache/libvmod_slash.so,ajcylwhs.so
Message from VCC-compiler:
FOUND VMOD in VEXT ../vext_cache/libvmod_slash.so,ajcylwhs.so
GOOD VMOD slash in VEXT ../vext_cache/libvmod_slash.so,ajcylwhs.so
Could not find VMOD proxyy
('/tmp/vcl-content1757856492' Line 13 Pos 8)
import proxyy;
-------######-

Running VCC-compiler failed, exited with 2
VCL compilation failed
```

## Backend unix socket paths
On the real vinyl servers the backends are unix sockets created by haproxy in
`/haproxy-unix-sockets`. Since those do not exist here, the validator creates
dummy socket files for the backend `.path` values found in the submitted VCL
so `vinyld` does not warn about them. Sockets are only created for the
duration of the request and are shared by concurrent requests using the same
name.

Since the VCL content is untrusted, the request is rejected with
`422 Unprocessable Entity` if:
* A `.path` is not a file directly in `/haproxy-unix-sockets`, its file name
  does not match `^[a-z0-9][a-z0-9_-]{0,127}$`, or it contains `.` or `..`
  path components.
* The VCL refers to more than 32 unique socket paths.
* `vinyld` still complains about a backend path after the sockets were created
  (e.g. a `.path` written in a way the validator did not recognize).
* The backend paths `vinyld` compiled, as found in its generated C code, are
  not exactly the paths the validator recognized in the VCL.

Only backend paths laid out the way sunet-cdn-manager generates them get a
socket: a line of its own holding `.path = "/haproxy-unix-sockets/...";`,
with any indentation. Anything else, such as a backend written on one line,
different spacing or
["long strings"](https://vinyl-cache.org/docs/9.1/reference/vcl.html#strings)
(`{"..."}` or `"""..."""`), gets none and is rejected, either
because `vinyld` complains about the missing socket or because the compiled
path is not one that was recognized. The latter also catches paths that
resolve to a socket created anyway, like
`{"/proc/self/root/haproxy-unix-sockets/..."}` or a socket created for another
request running at the same time. Lines are found with a text match, so such a
line inside a long string is also checked, and since it is not compiled as a
backend path the request is rejected.

## Development
### Tests
`go test ./...` runs the unit tests. The tests in `vinyld_test.go` need
`vinyld` and the `/haproxy-unix-sockets` directory, so they are skipped when
`vinyld` is missing unless `SUNET_VCL_VALIDATOR_REQUIRE_VINYLD` is set. The
container build runs all tests against the real `vinyld` with that variable
set, so a base image update that changes how `vinyld` reports or compiles
backend paths fails the build instead of silently disabling the checks.

### Formatting and linting
When working with this code at least the following tools are expected to be
run at the top level directory prior to commiting:

* `gofumpt -l -w .` (see [gofumpt](https://github.com/mvdan/gofumpt))
* `go vet ./...`
* `staticcheck ./...` (see [staticcheck](https://staticcheck.io))
* `gosec ./...` (see [gosec](https://github.com/securego/gosec))
* `golangci-lint run` (see [golangci-lint](https://golangci-lint.run))
