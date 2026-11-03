package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/YuukiFST/perfwatch/internal/config"
)

func runServe(ctx context.Context, streams Streams, args []string) (err error) {
	fs := newFlagSet("serve")
	port := fs.Int("port", 7313, "listen on this port of 127.0.0.1 (0 picks a free one)")
	if err := parseFlags(fs, streams, args); err != nil {
		return err
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(streams.Stderr, "invalid value %d for flag -port: want 0 to 65535\n", *port)
		fs.SetOutput(streams.Stderr)
		fs.Usage()
		return errUsage
	}
	dir, err := dataDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		return err
	}
	db, err := openForReading(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	// Loopback only: the dashboard shows every program the machine ran, to no one else.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	// SAFETY: net.Listen on "tcp" returns a *net.TCPListener, whose Addr is a *net.TCPAddr.
	// Verified by: TestServe, which starts serve on port 0 and reads the port it printed.
	addr := ln.Addr().(*net.TCPAddr)
	srv := &http.Server{
		Handler:           newDashboard(db, cfg.Peaks, addr.Port),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(streams.Stdout, "perfwatch dashboard on http://127.0.0.1:%d/ (Ctrl+C stops it)\n", addr.Port)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
