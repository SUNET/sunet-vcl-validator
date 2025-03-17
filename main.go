package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/justinas/alice"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

func aliceRequestLoggerChain(zlog zerolog.Logger) alice.Chain {
	chain := alice.New()

	chain = chain.Append(hlog.NewHandler(zlog))

	chain = chain.Append(hlog.AccessHandler(func(r *http.Request, status, size int, duration time.Duration) {
		hlog.FromRequest(r).Info().
			Str("method", r.Method).
			Stringer("url", r.URL).
			Int("status", status).
			Int("size", size).
			Dur("duration", duration).
			Msg("")
	}))

	chain = chain.Append(hlog.RemoteIPHandler("ip"))
	chain = chain.Append(hlog.UserAgentHandler("user_agent"))
	chain = chain.Append(hlog.RefererHandler("referer"))
	chain = chain.Append(hlog.RequestIDHandler("req_id", "Request-Id"))

	return chain
}

func validateVCL(w http.ResponseWriter, r *http.Request) {
	logger := hlog.FromRequest(r)

	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vcl, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unable to read VCL", http.StatusBadRequest)
		return
	}

	tmpFh, err := os.CreateTemp("", "vcl-content")
	if err != nil {
		http.Error(w, "unable to create tmp file", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpFh.Name())

	logger.Info().Str("filename", tmpFh.Name()).Msg("created fh")

	_, err = tmpFh.Write(vcl)
	if err != nil {
		http.Error(w, "unable to write to tmp file", http.StatusInternalServerError)
		return
	}

	err = tmpFh.Close()
	if err != nil {
		http.Error(w, "unable to close tmp file", http.StatusInternalServerError)
		return
	}

	var stderr strings.Builder

	cmd := exec.Command("/usr/sbin/varnishd", "-E", "/usr/lib/varnish/vmods/libvmod_slash.so", "-s", "fellow=fellow,/cache/fellow-storage,1MB,1MB,1MB", "-C", "-f", tmpFh.Name()) // #nosec G204 -- tmpFh is controlled by us.
	// The resulting C code (or error) is printed to stderr
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		logger.Err(err).Str("stderr", stderr.String()).Msg("varnishd failed")
		http.Error(w, stderr.String(), http.StatusUnprocessableEntity)
		return
	}
}

func setupDummyUDS() error {
	// These unix sockets are expected to exist on the real varnish servers
	// (created by haproxy). If these do not exist we get warning messages looking
	// like this when validating the VCL:
	// ===
	// Backend path: Cannot stat: No such file or directory
	// ('/tmp/vcl-content3464804458' Line 17 Pos 11)
	//   .path = "/shared/unix-sockets/haproxy_https";
	// ----------#######################-
	//
	// (That was just a warning)
	// Backend path: Cannot stat: No such file or directory
	// ('/tmp/vcl-content3464804458' Line 21 Pos 11)
	//   .path = "/shared/unix-sockets/haproxy_http";
	// ----------######################-
	//
	// (That was just a warning)
	// ===
	// The validation still succeeds but when there are other actual errors having
	// to disregard these are annoying. For this reason create some dummy files. We
	// expect the Dockerfile to make sure the /shared/unix-sockets
	// directory tree is available and writeable for us.
	unixSocketPaths := []string{
		"/shared/unix-sockets/haproxy_https",
		"/shared/unix-sockets/haproxy_http",
	}

	for _, socketPath := range unixSocketPaths {
		// We only need a file entry on disk that looks like a socket
		// file, not a properly initialized unix socket, so instead of using
		// something like net.Listen() just use Mknod(). It needs to be
		// an actual socket file otherwise varnishd will treat it as a
		// error instead of a warning, throwing this error:
		// "Backend path: Not a socket:"
		err := syscall.Mknod(socketPath, syscall.S_IFSOCK|0o600, 0)
		if err != nil {
			// We do not do any cleanup of these files so if they
			// already exist that is OK
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
	}

	return nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "sunet-vcl-validator").
		Logger()

	err := setupDummyUDS()
	if err != nil {
		log.Fatal(err)
	}

	// Exit gracefully on SIGINT or SIGTERM
	go func(logger zerolog.Logger, cancel context.CancelFunc) {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		s := <-sigCh
		logger.Info().Str("signal", s.String()).Msg("received signal")
		cancel()
	}(logger, cancel)

	c := aliceRequestLoggerChain(logger)

	loggingMux := c.Then(http.DefaultServeMux)

	addr := ":8888"

	srv := &http.Server{
		Addr:           addr,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
		Handler:        loggingMux,
	}

	http.HandleFunc("/validate-vcl", validateVCL)

	shutdownDelay := time.Second * 3

	// Handle graceful shutdown of HTTP server when receiving signal
	idleConnsClosed := make(chan struct{})

	go func(ctx context.Context, logger zerolog.Logger, shutdownDelay time.Duration) {
		<-ctx.Done()

		logger.Info().Msgf("sleeping for %s then calling Shutdown()", shutdownDelay)
		time.Sleep(shutdownDelay)
		if err := srv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			logger.Err(err).Msg("HTTP server Shutdown failure")
		}
		close(idleConnsClosed)
	}(ctx, logger, shutdownDelay)

	logger.Info().Str("addr", addr).Msg("starting server")
	err = srv.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}

	<-idleConnsClosed
}
