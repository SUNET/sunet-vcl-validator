package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

// backendsVCL returns one backend per path, laid out the way
// sunet-cdn-manager generates them in vcl-preamble.vcl.
func backendsVCL(paths ...string) string {
	var b strings.Builder
	for i, path := range paths {
		fmt.Fprintf(&b, "backend b%d {\n  .path = \"%s\";\n}\n", i, path)
	}
	return b.String()
}

func TestSocketNamesFromVCL(t *testing.T) {
	long128 := "a" + strings.Repeat("b", 127)
	long129 := "a" + strings.Repeat("b", 128)

	tests := []struct {
		name    string
		vcl     string
		want    []string
		wantErr string // substring expected in the error, empty means no error
	}{
		{
			name: "no backends",
			vcl:  "vcl 4.1;\nbackend default none;\n",
			want: nil,
		},
		{
			name: "manager layout",
			vcl:  "backend default_https {\n  .path = \"/haproxy-unix-sockets/haproxy_default_https\";\n}\n",
			want: []string{"haproxy_default_https"},
		},
		{
			name: "any indentation",
			vcl:  ".path = \"/haproxy-unix-sockets/x\";\n\t.path = \"/haproxy-unix-sockets/y\";\n",
			want: []string{"x", "y"},
		},
		// Only a whole line laid out the way sunet-cdn-manager writes it
		// is recognized. Anything else gets no socket and is rejected
		// after compiling, see TestVinyldRejectsUnscannedPath.
		{
			name: "backend on one line is not recognized",
			vcl:  `backend a { .path = "/haproxy-unix-sockets/x"; }`,
			want: nil,
		},
		{
			name: "no spaces around equals sign is not recognized",
			vcl:  "backend a {\n  .path=\"/haproxy-unix-sockets/x\";\n}\n",
			want: nil,
		},
		{
			name: "line break after equals sign is not recognized",
			vcl:  "backend a {\n  .path =\n  \"/haproxy-unix-sockets/x\";\n}\n",
			want: nil,
		},
		{
			name: "trailing comment is not recognized",
			vcl:  "backend a {\n  .path = \"/haproxy-unix-sockets/x\"; # comment\n}\n",
			want: nil,
		},
		{
			name: "long string is not recognized",
			vcl:  "backend a {\n  .path = {\"/haproxy-unix-sockets/x\"};\n}\n",
			want: nil,
		},
		{
			name: "triple quoted string is not recognized",
			vcl:  "backend a {\n  .path = \"\"\"/haproxy-unix-sockets/x\"\"\";\n}\n",
			want: nil,
		},
		{
			name: "whitespace after dot is not recognized",
			vcl:  "backend a {\n  . path = \"/haproxy-unix-sockets/x\";\n}\n",
			want: nil,
		},
		{
			name: "header named path is not a backend path",
			vcl:  "sub vcl_recv {\n  set req.http.path = \"/etc/passwd\";\n}\n",
			want: nil,
		},
		{
			name: "duplicates are returned once in first-seen order",
			vcl:  backendsVCL("/haproxy-unix-sockets/b", "/haproxy-unix-sockets/a", "/haproxy-unix-sockets/b"),
			want: []string{"b", "a"},
		},
		{
			name: "redundant slashes are cleaned",
			vcl:  backendsVCL("/haproxy-unix-sockets//x"),
			want: []string{"x"},
		},
		{
			name: "128 character name is allowed",
			vcl:  backendsVCL("/haproxy-unix-sockets/" + long128),
			want: []string{long128},
		},
		{
			name:    "129 character name is rejected",
			vcl:     backendsVCL("/haproxy-unix-sockets/" + long129),
			wantErr: long129,
		},
		{
			name:    "path outside socket dir",
			vcl:     backendsVCL("/etc/passwd"),
			wantErr: `"/etc/passwd"`,
		},
		{
			name:    "dot dot escaping socket dir",
			vcl:     backendsVCL("/haproxy-unix-sockets/../etc/x"),
			wantErr: `"/haproxy-unix-sockets/../etc/x"`,
		},
		{
			// Cleans to an allowed path, but the kernel resolves
			// ".." after following symlinks such as /proc/self/root,
			// so a cleaned path does not say where it really leads.
			name:    "dot dot that cleans to an allowed path",
			vcl:     backendsVCL("/proc/self/root/../../../haproxy-unix-sockets/x"),
			wantErr: `backend path "/proc/self/root/../../../haproxy-unix-sockets/x" not allowed: must not contain "." or ".." path components`,
		},
		{
			name:    "dot dot back into socket dir",
			vcl:     backendsVCL("/haproxy-unix-sockets/../haproxy-unix-sockets/x"),
			wantErr: `must not contain "." or ".." path components`,
		},
		{
			name:    "dot in socket dir",
			vcl:     backendsVCL("/haproxy-unix-sockets/./x"),
			wantErr: `must not contain "." or ".." path components`,
		},
		{
			name:    "subdirectory of socket dir",
			vcl:     backendsVCL("/haproxy-unix-sockets/sub/x"),
			wantErr: `"/haproxy-unix-sockets/sub/x"`,
		},
		{
			name:    "relative path",
			vcl:     backendsVCL("haproxy_x_http"),
			wantErr: `"haproxy_x_http"`,
		},
		{
			name:    "socket dir itself",
			vcl:     backendsVCL("/haproxy-unix-sockets/"),
			wantErr: `"/haproxy-unix-sockets/"`,
		},
		{
			name:    "sibling dir sharing prefix",
			vcl:     backendsVCL("/haproxy-unix-sockets-evil/x"),
			wantErr: `"/haproxy-unix-sockets-evil/x"`,
		},
		{
			name:    "uppercase in name",
			vcl:     backendsVCL("/haproxy-unix-sockets/Haproxy"),
			wantErr: `"/haproxy-unix-sockets/Haproxy"`,
		},
		{
			name:    "dot in name",
			vcl:     backendsVCL("/haproxy-unix-sockets/x.sock"),
			wantErr: `"/haproxy-unix-sockets/x.sock"`,
		},
		{
			name:    "leading dash in name",
			vcl:     backendsVCL("/haproxy-unix-sockets/-x"),
			wantErr: `"/haproxy-unix-sockets/-x"`,
		},
		{
			name:    "control characters are quoted in the error",
			vcl:     backendsVCL("/etc/x\ty\x01"),
			wantErr: `"/etc/x\ty\x01"`,
		},
		{
			name:    "invalid path after valid one is still rejected",
			vcl:     backendsVCL("/haproxy-unix-sockets/ok", "/tmp/x"),
			wantErr: `"/tmp/x"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := socketNamesFromVCL([]byte(tc.vcl))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %s, got names %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func vclWithSocketBackends(n int, name func(i int) string) string {
	paths := make([]string, n)
	for i := range n {
		paths[i] = "/haproxy-unix-sockets/" + name(i)
	}
	return backendsVCL(paths...)
}

func TestSocketNamesFromVCLCap(t *testing.T) {
	letters := func(i int) string { return fmt.Sprintf("s%02d", i) }

	got, err := socketNamesFromVCL([]byte(vclWithSocketBackends(32, letters)))
	if err != nil {
		t.Fatalf("32 unique sockets should be allowed: %s", err)
	}
	if len(got) != 32 {
		t.Fatalf("got %d names, want 32", len(got))
	}

	_, err = socketNamesFromVCL([]byte(vclWithSocketBackends(33, letters)))
	if err == nil {
		t.Fatal("33 unique sockets should be rejected")
	}

	// Repeating the same path many times only counts once.
	same := func(int) string { return "same" }
	got, err = socketNamesFromVCL([]byte(vclWithSocketBackends(100, same)))
	if err != nil {
		t.Fatalf("repeated identical paths should be allowed: %s", err)
	}
	if !slices.Equal(got, []string{"same"}) {
		t.Fatalf("got %q, want [same]", got)
	}
}

func isSocket(t *testing.T, path string) bool {
	t.Helper()
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("lstat %s: %s", path, err)
	}
	if fi.Mode().Type() != fs.ModeSocket {
		t.Fatalf("%s exists but is not a socket: %s", path, fi.Mode())
	}
	return true
}

func openTestSocketDir(t *testing.T, dir string) *socketDir {
	t.Helper()
	sd, err := openSocketDir(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("openSocketDir: %s", err)
	}
	t.Cleanup(func() { sd.close() })
	return sd
}

func TestSocketDirAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	sd := openTestSocketDir(t, dir)

	release, err := sd.acquire([]string{"a", "b"})
	if err != nil {
		t.Fatalf("acquire: %s", err)
	}
	if !isSocket(t, filepath.Join(dir, "a")) || !isSocket(t, filepath.Join(dir, "b")) {
		t.Fatal("sockets not created on acquire")
	}

	release()
	if isSocket(t, filepath.Join(dir, "a")) || isSocket(t, filepath.Join(dir, "b")) {
		t.Fatal("sockets not removed on release")
	}
}

func TestSocketDirOverlappingRequests(t *testing.T) {
	dir := t.TempDir()
	sd := openTestSocketDir(t, dir)

	releaseA, err := sd.acquire([]string{"shared"})
	if err != nil {
		t.Fatalf("acquire A: %s", err)
	}
	releaseB, err := sd.acquire([]string{"shared", "onlyb"})
	if err != nil {
		t.Fatalf("acquire B: %s", err)
	}

	releaseB()
	if !isSocket(t, filepath.Join(dir, "shared")) {
		t.Fatal("shared socket removed while A still holds it")
	}
	if isSocket(t, filepath.Join(dir, "onlyb")) {
		t.Fatal("onlyb socket not removed when B released it")
	}

	releaseA()
	if isSocket(t, filepath.Join(dir, "shared")) {
		t.Fatal("shared socket not removed after last release")
	}
}

func TestSocketDirConcurrent(t *testing.T) {
	dir := t.TempDir()
	sd := openTestSocketDir(t, dir)
	path := filepath.Join(dir, "shared")

	// acquireAndCheck holds the socket for one iteration. The deferred
	// release runs when it returns, so every iteration gets a chance to
	// create and remove the socket instead of piling up references.
	acquireAndCheck := func() error {
		release, err := sd.acquire([]string{"shared"})
		if err != nil {
			return err
		}
		defer release()

		fi, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("socket missing while held: %w", err)
		}
		if fi.Mode().Type() != fs.ModeSocket {
			return fmt.Errorf("%s is not a socket: %s", path, fi.Mode())
		}
		return nil
	}

	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 50 {
				if err := acquireAndCheck(); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if isSocket(t, path) {
		t.Fatal("socket left behind after all releases")
	}
}

func TestSocketDirAcquireExistingSocket(t *testing.T) {
	// A socket left over from a crash must not make acquire fail, and it
	// is removed by the release like any other.
	dir := t.TempDir()
	sd := openTestSocketDir(t, dir)
	release, err := sd.acquire([]string{"x"})
	if err != nil {
		t.Fatalf("acquire: %s", err)
	}
	// Simulate the count being lost (as after a crash) with the file
	// still on disk by acquiring through a second socketDir.
	sd2 := openTestSocketDirNoClean(t, dir)
	release2, err := sd2.acquire([]string{"x"})
	if err != nil {
		t.Fatalf("acquire with existing socket on disk: %s", err)
	}
	if !isSocket(t, filepath.Join(dir, "x")) {
		t.Fatal("socket missing after acquire")
	}
	release2()
	release()
}

func openTestSocketDirNoClean(t *testing.T, dir string) *socketDir {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := newSocketDir(root, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sd.close() })
	return sd
}

func TestSocketDirAcquireRejectsUnsafeNames(t *testing.T) {
	// Even if a bad name gets past socketNamesFromVCL, acquire must not
	// create anything outside the socket directory.
	parent := t.TempDir()
	dir := filepath.Join(parent, "sockets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sd := openTestSocketDir(t, dir)

	for _, name := range []string{"../escaped", "sub/x", "..", ".", "", "/abs"} {
		release, err := sd.acquire([]string{"ok", name})
		if err == nil {
			release()
			t.Errorf("acquire accepted unsafe name %q", name)
		}
	}
	if isSocket(t, filepath.Join(parent, "escaped")) {
		t.Fatal("socket created outside socket dir")
	}
	if isSocket(t, filepath.Join(dir, "ok")) {
		t.Fatal("failed acquire left a socket behind")
	}
}

func TestSocketDirAcquireRefusesNonSocket(t *testing.T) {
	dir := t.TempDir()
	sd := openTestSocketDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := sd.acquire([]string{"x"})
	if err == nil {
		release()
		t.Fatal("acquire accepted a regular file in place of a socket")
	}
}

func TestSocketDirPinnedAtOpen(t *testing.T) {
	// Swapping the directory path for a symlink after startup must not
	// redirect where sockets are created.
	parent := t.TempDir()
	dir := filepath.Join(parent, "sockets")
	elsewhere := filepath.Join(parent, "elsewhere")
	for _, d := range []string{dir, elsewhere} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sd := openTestSocketDir(t, dir)

	moved := filepath.Join(parent, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}

	release, err := sd.acquire([]string{"x"})
	if err != nil {
		t.Fatalf("acquire: %s", err)
	}
	defer release()
	if isSocket(t, filepath.Join(elsewhere, "x")) {
		t.Fatal("socket followed the swapped-in symlink")
	}
	if !isSocket(t, filepath.Join(moved, "x")) {
		t.Fatal("socket not created in the originally opened directory")
	}
}

func TestOpenSocketDirRemovesLeftoverSockets(t *testing.T) {
	dir := t.TempDir()
	sd := openTestSocketDirNoClean(t, dir)
	if _, err := sd.acquire([]string{"leftover"}); err != nil {
		t.Fatal(err)
	}
	if !isSocket(t, filepath.Join(dir, "leftover")) {
		t.Fatal("setup: leftover socket not created")
	}
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	openTestSocketDir(t, dir)

	if isSocket(t, filepath.Join(dir, "leftover")) {
		t.Fatal("leftover socket not removed at open")
	}
	if _, err := os.Stat(regular); err != nil {
		t.Fatalf("non-socket file was touched: %s", err)
	}
}

func TestBackendPathWarnings(t *testing.T) {
	// Captured from vinyld 9.1.0 stderr for a VCL with two backends whose
	// .path does not exist, cut off a few lines into the generated C code
	// that follows the compiler messages.
	missing := `EEE </usr/lib/vinyl-cache/extension-vmods/libvmod_slash.so>
eee </usr/lib/vinyl-cache/extension-vmods/libvmod_slash.so>
ee2 vext_cache/libvmod_slash.so,zkacynvm.so
Message from VCC-compiler:
FOUND VMOD in VEXT ../vext_cache/libvmod_slash.so,zkacynvm.so
GOOD VMOD slash in VEXT ../vext_cache/libvmod_slash.so,zkacynvm.so
Backend path: Cannot stat: No such file or directory
('/c/missing.vcl' Line 2 Pos 30)
backend b { . path /* x */ = "/haproxy-unix-sockets/missing_b"; }
-----------------------------#################################---

(That was just a warning)
Backend path: Cannot stat: No such file or directory
('/c/missing.vcl' Line 3 Pos 21)
backend c { .path = {"/haproxy-unix-sockets/missing_c"}; }
--------------------###################################---

(That was just a warning)
/* ---===### include/vdef.h ###===--- */

/*-
 * Copyright (c) 2006 Verdens Gang AS
`
	want := `Backend path: Cannot stat: No such file or directory
('/c/missing.vcl' Line 2 Pos 30)
backend b { . path /* x */ = "/haproxy-unix-sockets/missing_b"; }
-----------------------------#################################---

Backend path: Cannot stat: No such file or directory
('/c/missing.vcl' Line 3 Pos 21)
backend c { .path = {"/haproxy-unix-sockets/missing_c"}; }
--------------------###################################---

`
	if got := backendPathWarnings(missing); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// Excerpts captured from vinyld 9.1.0 stderr for a VCL that compiles
	// cleanly but has "Backend path:" in its strings, first where the VCL
	// source is embedded in the generated C code and then where the
	// strings are used.
	clean := `Message from VCC-compiler:
FOUND VMOD in VEXT ../vext_cache/libvmod_slash.so,zlwhybtg.so
GOOD VMOD slash in VEXT ../vext_cache/libvmod_slash.so,zlwhybtg.so
	"sub vcl_synth {\n"
	"  set resp.body = {\"\n"
	"Backend path: in a long string\"};\n"
	"  set resp.http.x = \"Backend path: in a header\";\n"
	"  return (deliver);\n"
	"}\n"
      VRT_l_resp_body(ctx, 
        LBODY_SET_STRING, 0, TOSTRAND("\n"
        "Backend path: in a long string")
      );
      END_;
      VRT_SetHdr(ctx, &VGC_HDR_RESP_x,
        0,
        TOSTRAND("Backend path: in a header")
      );
`
	if got := backendPathWarnings(clean); got != "" {
		t.Errorf("false positive on output without a warning: %q", got)
	}
}

func TestCheckCompiledBackendPaths(t *testing.T) {
	// Excerpts captured from vinyld 9.1.0 stderr for a VCL with:
	//
	//   - a backend in the manager layout
	//   - a backend aliasing it with a "long string" path
	//   - the comment: # .uds_path = "/fake";
	//
	// The excerpts are the struct member declaration from the included
	// headers, the VCL source embedded as C strings and the endpoints of
	// the two backends.
	//
	// For "long string" see "Strings" in vcl(7):
	// https://vinyl-cache.org/docs/9.1/reference/vcl.html#strings
	cCode := `struct vrt_endpoint {
	unsigned			magic;
#define VRT_ENDPOINT_MAGIC		0xcc419347
	VCL_IP				ipv4;
	VCL_IP				ipv6;
	const char			*uds_path;
	int				preamble;
};
	"backend default {\n"
	"  .path = \"/haproxy-unix-sockets/haproxy_default_https\";\n"
	"}\n"
	"backend sneaky {\n"
	"  .path = {\"/proc/self/root/haproxy-unix-sockets/haproxy_default_https\"};\n"
	"}\n"
	"sub vcl_recv {\n"
	"  # .uds_path = \"/fake\";\n"
static const struct vrt_endpoint vgc_dir_ep_vgc_backend_default = {
	.magic = VRT_ENDPOINT_MAGIC,
	.uds_path = "/haproxy-unix-sockets/haproxy_default_https",
	.ipv4 = (void *) 0,
	.ipv6 = (void *) 0,
};

static const struct vrt_endpoint vgc_dir_ep_vgc_backend_sneaky = {
	.magic = VRT_ENDPOINT_MAGIC,
	.uds_path = "/proc/self/root/haproxy-unix-sockets/haproxy_default_https",
	.ipv4 = (void *) 0,
	.ipv6 = (void *) 0,
};
`
	// The same output without the sneaky backend.
	cleanCCode := strings.Replace(cCode, `	.uds_path = "/proc/self/root/haproxy-unix-sockets/haproxy_default_https",
`, "", 1)

	tests := []struct {
		name    string
		vcl     string
		cCode   string
		wantErr string // empty for no error
	}{
		{
			name:  "compiled paths match recognized paths",
			vcl:   backendsVCL("/haproxy-unix-sockets/haproxy_default_https"),
			cCode: cleanCCode,
		},
		{
			name:  "same path in two backends",
			vcl:   backendsVCL("/haproxy-unix-sockets/haproxy_default_https", "/haproxy-unix-sockets/haproxy_default_https"),
			cCode: cleanCCode + "\t.uds_path = \"/haproxy-unix-sockets/haproxy_default_https\",\n",
		},
		{
			name:    "compiled path that was not recognized",
			vcl:     backendsVCL("/haproxy-unix-sockets/haproxy_default_https"),
			cCode:   cCode,
			wantErr: `backend path "/proc/self/root/haproxy-unix-sockets/haproxy_default_https" not allowed: must be a file directly in /haproxy-unix-sockets`,
		},
		{
			// The only recognized line is inside a "long string",
			// so it is just header content to vinyld, but it gets
			// the socket created. The real backend .path is a
			// "long string" the scan does not recognize, spelled
			// differently but resolving to that socket, so vinyld
			// does not complain. Only the exact recognized string
			// is allowed.
			name: "compiled path differs from recognized path",
			vcl: `vcl 4.1;
backend default {
  .path = {"/haproxy-unix-sockets/haproxy_default_https"};
}
sub vcl_deliver {
  set resp.http.x = {"
  .path = "/haproxy-unix-sockets//haproxy_default_https";
"};
}
`,
			// Excerpts captured from vinyld 9.1.0 stderr for the
			// VCL above: the embedded VCL source, the backend
			// endpoint and the header being set.
			cCode: `    /* "/tmp/a.vcl" */
	"vcl 4.1;\n"
	"backend default {\n"
	"  .path = {\"/haproxy-unix-sockets/haproxy_default_https\"};\n"
	"}\n"
	"sub vcl_deliver {\n"
	"  set resp.http.x = {\"\n"
	"  .path = \"/haproxy-unix-sockets//haproxy_default_https\";\n"
	"\"};\n"
	"}\n"
	"",
static const struct vrt_endpoint vgc_dir_ep_vgc_backend_default = {
	.magic = VRT_ENDPOINT_MAGIC,
	.uds_path = "/haproxy-unix-sockets/haproxy_default_https",
	.ipv4 = (void *) 0,
	.ipv6 = (void *) 0,
};
      VRT_SetHdr(ctx, &VGC_HDR_RESP_x,
        0,
        TOSTRAND("\n"
        "  .path = \"/haproxy-unix-sockets//haproxy_default_https\";\n"
        "")
      );
`,
			wantErr: `backend path "/haproxy-unix-sockets/haproxy_default_https" must be written as a line of its own in a backend: .path = "/haproxy-unix-sockets/haproxy_default_https";`,
		},
		{
			name:    "recognized path that was not compiled",
			vcl:     backendsVCL("/haproxy-unix-sockets/haproxy_default_https", "/haproxy-unix-sockets/haproxy_extra_https"),
			cCode:   cleanCCode,
			wantErr: `backend path "/haproxy-unix-sockets/haproxy_extra_https" is not used by a backend`,
		},
		{
			// Same path as in the VCL, but as if a future vinyld
			// renamed the C struct member .uds_path to
			// .socket_path, so no compiled paths are found. Every
			// VCL with a backend path must then be rejected rather
			// than the check silently passing.
			name:    "no compiled paths found",
			vcl:     backendsVCL("/haproxy-unix-sockets/haproxy_default_https"),
			cCode:   "\t.socket_path = \"/haproxy-unix-sockets/haproxy_default_https\",\n",
			wantErr: `backend path "/haproxy-unix-sockets/haproxy_default_https" is not used by a backend`,
		},
		{
			// Any line setting .uds_path counts, even one laid out
			// differently than vinyld does it.
			name:    "compiled path in a different layout",
			vcl:     backendsVCL("/haproxy-unix-sockets/haproxy_default_https"),
			cCode:   cleanCCode + "  .uds_path=\"/haproxy-unix-sockets/haproxy_default_https\",\n",
			wantErr: `unexpected backend path line in vinyld output: "  .uds_path=\"/haproxy-unix-sockets/haproxy_default_https\","`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCompiledBackendPaths([]byte(tc.vcl), tc.cCode)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %s", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
