package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// requireVinyldEnv makes the tests in this file fail instead of skip when
// vinyld is not available. It is set when the tests run as part of the
// container build so they can not be skipped silently there.
const requireVinyldEnv = "SUNET_VCL_VALIDATOR_REQUIRE_VINYLD"

// newVinyldTestHandler returns the real validation handler and the real
// socket directory backing it, or skips the test if vinyld is not available.
func newVinyldTestHandler(t *testing.T) (http.Handler, *socketDir) {
	t.Helper()

	_, err := os.Stat(vinylCmd)
	if err != nil {
		if os.Getenv(requireVinyldEnv) != "" {
			t.Fatalf("%s is set but vinyld is not available: %s", requireVinyldEnv, err)
		}
		t.Skipf("vinyld not available (%s), set %s to make this an error", err, requireVinyldEnv)
	}

	sockets, err := openSocketDir(socketDirPath, zerolog.Nop())
	if err != nil {
		t.Fatalf("openSocketDir: %s", err)
	}
	t.Cleanup(func() {
		entries, err := fs.ReadDir(sockets.root.FS(), ".")
		if err != nil {
			t.Errorf("unable to read socket dir: %s", err)
		}
		for _, e := range entries {
			t.Errorf("socket dir not empty after test: %s", e.Name())
		}
		sockets.close()
	})

	return newValidateVCLHandler(sockets), sockets
}

func postVCL(t *testing.T, h http.Handler, vcl string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/validate-vcl", strings.NewReader(vcl))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// managerStyleVCL mirrors what sunet-cdn-manager generates for a default and
// one conditional origin group: the backends from vcl-preamble.vcl and the
// backend selection from buildBackendSelection() in vcl_recv, followed by
// tenant VCL. Every backend has to be used since vinyld rejects VCL with
// unused backends (vcc_feature err_unref).
func managerStyleVCL(tenant string) string {
	return `vcl 4.1;
import proxy;
import slash;
backend default none;
backend default_https {
  .path = "/haproxy-unix-sockets/haproxy_default_https";
}
backend default_http {
  .path = "/haproxy-unix-sockets/haproxy_default_http";
}
backend images_https {
  .path = "/haproxy-unix-sockets/haproxy_images_https";
}
backend images_http {
  .path = "/haproxy-unix-sockets/haproxy_images_http";
}
sub vcl_recv {
  if (proxy.is_ssl()) {
    set req.http.X-Forwarded-Proto = "https";
    if (req.url ~ "^/img") { # origin group "images" (1/1)
      set req.backend_hint = images_https;
    } else { # default origin group "default"
      set req.backend_hint = default_https;
    }
  } else {
    set req.http.X-Forwarded-Proto = "http";
    if (req.url ~ "^/img") { # origin group "images" (1/1)
      set req.backend_hint = images_http;
    } else { # default origin group "default"
      set req.backend_hint = default_http;
    }
  }
}
` + tenant
}

func TestVinyldAcceptsOriginGroupSockets(t *testing.T) {
	h, _ := newVinyldTestHandler(t)

	code, body := postVCL(t, h, managerStyleVCL(""))
	if code != http.StatusOK {
		t.Fatalf("got %d, want 200, body:\n%s", code, body)
	}
}

func TestVinyldErrorHasNoSocketWarnings(t *testing.T) {
	// When the VCL has a real error, the output must not also contain
	// complaints about the origin group sockets.
	h, _ := newVinyldTestHandler(t)

	code, body := postVCL(t, h, managerStyleVCL("sub vcl_deliver { set resp.http.x = undefined_symbol; }\n"))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422, body:\n%s", code, body)
	}
	if !strings.Contains(body, "undefined_symbol") {
		t.Fatalf("error does not mention the actual problem:\n%s", body)
	}
	if strings.Contains(body, "Backend path") {
		t.Fatalf("error contains socket complaints:\n%s", body)
	}
}

// sneakyBackendVCL returns manager style VCL with an extra backend using
// the given .path attribute line.
func sneakyBackendVCL(pathLine string) string {
	return managerStyleVCL("backend sneaky {\n  " + pathLine + `
}
sub vcl_backend_fetch {
  if (bereq.http.x-sneaky) {
    set bereq.backend = sneaky;
  }
}
`)
}

func TestVinyldRejectsUnscannedPath(t *testing.T) {
	// socketNamesFromVCL only recognizes the plain string form of .path
	// that sunet-cdn-manager generates, so these paths get no socket of
	// their own. The VCL must still be rejected, either because vinyld
	// complains about a missing socket or because the path it compiled
	// was not one we recognized, even when the path resolves to a socket
	// we did create. Each case checks which of the two rejected it, so
	// this test fails if vinyld changes how it reports backend paths or
	// how its generated C code sets them, even though the other check
	// would still reject the VCL.
	h, _ := newVinyldTestHandler(t)

	tests := []struct {
		name string
		path string // the .path attribute line in the backend
		want string // expected in the response
		// warning is true if the path leads to no socket, so vinyld
		// must complain about it, and false if it resolves to one of
		// our sockets, so the compiled path check must reject it.
		warning bool
	}{
		{
			name:    "comment between tokens",
			path:    `. path /* hidden */ = "/vinyl-unix-sockets/vinyl";`,
			want:    "/vinyl-unix-sockets/vinyl",
			warning: true,
		},
		{
			name:    "long string",
			path:    `.path = {"/vinyl-unix-sockets/vinyl"};`,
			want:    "/vinyl-unix-sockets/vinyl",
			warning: true,
		},
		{
			name:    "triple quoted string",
			path:    `.path = """/vinyl-unix-sockets/vinyl""";`,
			want:    "/vinyl-unix-sockets/vinyl",
			warning: true,
		},
		{
			// Allowed directory and name, but not written the way
			// the manager writes it.
			name:    "long string to unknown origin group socket",
			path:    `.path = {"/haproxy-unix-sockets/haproxy_nosuchgroup_https"};`,
			want:    "/haproxy-unix-sockets/haproxy_nosuchgroup_https",
			warning: true,
		},
		{
			// Resolves to the default origin group socket created
			// for this request, so vinyld does not complain.
			name: "alias through /proc",
			path: `.path = {"/proc/self/root/haproxy-unix-sockets/haproxy_default_https"};`,
			want: "/proc/self/root/haproxy-unix-sockets/haproxy_default_https",
		},
		{
			name: "alias with redundant slash",
			path: `.path = {"/haproxy-unix-sockets//haproxy_default_https"};`,
			want: "/haproxy-unix-sockets//haproxy_default_https",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vcl := sneakyBackendVCL(tc.path)
			names, err := socketNamesFromVCL([]byte(vcl))
			if err != nil {
				t.Fatalf("test VCL must get past the scan to exercise the backstop: %s", err)
			}
			if len(names) != 4 {
				t.Fatalf("scan unexpectedly found the path: %q", names)
			}

			code, body := postVCL(t, h, vcl)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("got %d, want 422, body:\n%s", code, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Fatalf("rejection does not show the offending path:\n%s", body)
			}
			if strings.Contains(body, "include/vdef.h") {
				t.Fatalf("rejection contains generated C code:\n%s", body)
			}

			// The literal vinyld wording on purpose, not
			// backendPathWarningRe, so a wording change fails here.
			gotWarning := strings.Contains(body, "\nBackend path: Cannot stat")
			if tc.warning && !gotWarning {
				t.Fatalf("not rejected because of the vinyld warning:\n%s", body)
			}
			if !tc.warning && gotWarning {
				t.Fatalf("unexpected vinyld warning, the compiled path check should reject this:\n%s", body)
			}
		})
	}
}

func TestVinyldRejectsPathBorrowedFromOverlappingRequest(t *testing.T) {
	// Another request currently holds a socket that this VCL refers to
	// without the scan recognizing it. Whether this VCL is accepted must
	// not depend on what other requests are doing at the same time.
	h, sockets := newVinyldTestHandler(t)

	release, err := sockets.acquire([]string{"haproxy_other_https"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	vcl := sneakyBackendVCL(`.path = {"/haproxy-unix-sockets/haproxy_other_https"};`)
	code, body := postVCL(t, h, vcl)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422, body:\n%s", code, body)
	}
	if !strings.Contains(body, "/haproxy-unix-sockets/haproxy_other_https") {
		t.Fatalf("rejection does not show the offending path:\n%s", body)
	}
}

func TestVinyldRejectsRecognizedPathOutsideBackend(t *testing.T) {
	// A line in the manager layout that is not a backend .path, here in a
	// "long string" (see "Strings" in vcl(7):
	// https://vinyl-cache.org/docs/9.1/reference/vcl.html#strings), gets
	// a socket but is not compiled as a backend path.
	h, _ := newVinyldTestHandler(t)

	vcl := managerStyleVCL(`sub vcl_deliver {
  set resp.http.x = {"
  .path = "/haproxy-unix-sockets/haproxy_extra_https";
"};
}
`)
	code, body := postVCL(t, h, vcl)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422, body:\n%s", code, body)
	}
	if !strings.Contains(body, "/haproxy-unix-sockets/haproxy_extra_https") {
		t.Fatalf("rejection does not show the offending path:\n%s", body)
	}
}
