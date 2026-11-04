package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/justinas/alice"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

const vinylCmd = "/usr/sbin/vinyld"

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

func newValidateVCLHandler(sockets *socketDir) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		// Fake the unix sockets the VCL refers to so vinyld does not
		// complain about them, rejecting any path that would not exist
		// on the real vinyl servers.
		socketNames, err := socketNamesFromVCL(vcl)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}

		releaseSockets, err := sockets.acquire(socketNames)
		if err != nil {
			logger.Err(err).Msg("unable to create sockets")
			http.Error(w, "unable to create sockets", http.StatusInternalServerError)
			return
		}
		defer releaseSockets()

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

		cmd := exec.Command(vinylCmd, "-E", "/usr/lib/vinyl-cache/extension-vmods/libvmod_slash.so", "-s", "fellow=fellow,/cache/fellow-storage,1MB,1MB,1MB", "-C", "-f", tmpFh.Name()) // #nosec G204 -- tmpFh is controlled by us.
		// The resulting C code (or error) is printed to stderr
		cmd.Stderr = &stderr

		err = cmd.Run()
		if err != nil {
			http.Error(w, stderr.String(), http.StatusUnprocessableEntity)
			return
		}

		// Every allowed backend path has a socket by now, so any
		// remaining complaint is about a path socketNamesFromVCL did not
		// find (e.g. ". path /* comment */ = ...") and that would not
		// work on the real vinyl servers.
		warnings := backendPathWarnings(stderr.String())
		if warnings != "" {
			http.Error(w, fmt.Sprintf("backend path not allowed: must be a file directly in %s with a name matching %s, written as a line of its own in a backend: .path = \"%s/<name>\";\n\n%s", socketDirPath, socketNameRe, socketDirPath, warnings), http.StatusUnprocessableEntity)
			return
		}

		// No complaint does not prove the paths are the ones we
		// checked: an unrecognized path can resolve to one of our
		// sockets, so compare with what vinyld actually compiled.
		err = checkCompiledBackendPaths(vcl, stderr.String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
	}
}

func getVinyldVersion() (string, string, error) {
	var stderr strings.Builder
	cmd := exec.Command(vinylCmd, "-V")
	// The vinyld version info strings are printed to stderr, e.g.:
	// ===
	// vinyld (vinyl-cache-9.1.0 revision 7c558af697ded4f5461ff97d83b2febc8d647d50)
	// Copyright (c) 2006 Verdens Gang AS
	// Copyright (c) 2006-2026 Varnish Software
	// Copyright 2010-2026 UPLEX - Nils Goroll Systemoptimierung
	// Copyright 2026 The Vinyl Cache Project
	// ===
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", "", fmt.Errorf("vinyld -V failed: %w (stderr: %q)", err, stderr.String())
	}

	line, _, _ := strings.Cut(stderr.String(), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", fmt.Errorf("vinyld -V produced no version line")
	}

	after, found := strings.CutPrefix(line, "vinyld (vinyl-cache-")
	if !found {
		return "", "", fmt.Errorf("unexpected vinyld version format: %q", line)
	}

	version, _, found := strings.Cut(after, " ")
	if !found {
		return "", "", fmt.Errorf("unexpected vinyld version format (no space after version): %q", line)
	}

	return line, version, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "sunet-vcl-validator").
		Str("go", "sunet-vcl-validator").
		Str("go_version", runtime.Version()).
		Logger()

	sockets, err := openSocketDir(socketDirPath, logger)
	if err != nil {
		logger.Fatal().Err(err).Str("dir", socketDirPath).Msg("unable to open socket directory")
	}
	defer sockets.close()

	vinyldFullVersion, vinylVersion, err := getVinyldVersion()
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to get vinyl version")
	}

	logger = logger.With().
		Str("vinyl_version", vinylVersion).
		Logger()

	// Log the complete vinyld version line at startup so we can see the
	// full output as well.
	logger.Info().Msg(vinyldFullVersion)

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

	http.HandleFunc("/validate-vcl", newValidateVCLHandler(sockets))

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
