# sunet-vcl-validator
This is the SUNET VCL validator server which allows you to POST a Varnish VCL
file and have it validate the contents. Created as a supporting service to
https://github.com/SUNET/sunet-cdn-manager which needs a way to validate VCL
submitted to it prior to adding it to the database.

## Running
The service needs access to a `varnishd` binary for the actual validation so
running it as a container is probably the easiest.
```
docker build -t sunet-vcl-validator:latest .
docker run -p 127.0.0.1:8888:8888 -it --rm sunet-vcl-validator:latest
```

## Usage
Given a VCL file you can submit it to the running service. A `200 OK` response
means the VCL file is OK, a `422 Unprocessable Entity` means it is invalid.

Example OK VCL:
```
$ curl -i --data-binary @varnish.vcl 127.0.0.1:8888/validate-vcl
HTTP/1.1 200 OK
Request-Id: cuh2la0f9mos73cu60k0
Date: Tue, 04 Feb 2025 14:56:41 GMT
Content-Length: 0
```

Example invalid VCL:
```
$ curl -i --data-binary @varnish-broken.vcl 127.0.0.1:8888/validate-vcl
HTTP/1.1 422 Unprocessable Entity
Content-Type: text/plain; charset=utf-8
Request-Id: cuh2lhof9mos73cu60kg
X-Content-Type-Options: nosniff
Date: Tue, 04 Feb 2025 14:57:11 GMT
Content-Length: 462

EEE </usr/lib/varnish/vmods/libvmod_slash.so>
eee </usr/lib/varnish/vmods/libvmod_slash.so>
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

## Development
### Formatting and linting
When working with this code at least the following tools are expected to be
run at the top level directory prior to commiting:

* `gofumpt -l -w .` (see [gofumpt](https://github.com/mvdan/gofumpt))
* `go vet ./...`
* `staticcheck ./...` (see [staticcheck](https://staticcheck.io))
* `gosec ./...` (see [gosec](https://github.com/securego/gosec))
* `golangci-lint run` (see [golangci-lint](https://golangci-lint.run))
