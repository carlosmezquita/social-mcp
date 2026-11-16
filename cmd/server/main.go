package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"social-mcp/internal/app"
)

func run(log *slog.Logger) error {
	syscall.Umask(0077)
	cfg, err := app.LoadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) == 1 || os.Args[1] == "quarantine-restored" || os.Args[1] == "cleanup-media" {
		if err = os.MkdirAll(filepath.Dir(cfg.DB), 0700); err != nil {
			return err
		}
		suffix := ".lock"
		flags := syscall.LOCK_EX | syscall.LOCK_NB
		if len(os.Args) > 1 && os.Args[1] == "cleanup-media" {
			suffix = ".maintenance"
			flags = syscall.LOCK_EX
		}
		lock, err := os.OpenFile(cfg.DB+suffix, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err = syscall.Flock(int(lock.Fd()), flags); err != nil {
			return errors.New("a server already owns this database")
		}
	}
	store, err := app.Open(ctx, cfg.DB, cfg.Media)
	if err != nil {
		return err
	}
	defer store.DB.Close()
	service := app.New(cfg, store, log)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup":
			if len(os.Args) != 3 {
				return errors.New("usage: social-mcp backup /absolute/snapshot.db")
			}
			return service.Backup(ctx, os.Args[2])
		case "verify":
			if len(os.Args) != 2 {
				return errors.New("usage: social-mcp verify")
			}
			return service.Verify(ctx)
		case "quarantine-restored":
			if len(os.Args) != 2 {
				return errors.New("usage: social-mcp quarantine-restored")
			}
			return store.QuarantineRestored(ctx)
		case "copy-snapshot-media":
			if len(os.Args) != 3 {
				return errors.New("usage: social-mcp copy-snapshot-media /absolute/source/media")
			}
			return store.CopySnapshotMedia(ctx, os.Args[2])
		case "cleanup-media":
			if len(os.Args) != 2 {
				return errors.New("usage: social-mcp cleanup-media")
			}
			count, err := store.Cleanup(ctx, 7*24*time.Hour)
			if err == nil {
				log.Info("unused media removed", "count", count)
			}
			return err
		default:
			return errors.New("commands: backup, verify, cleanup-media, quarantine-restored, copy-snapshot-media")
		}
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Addr: cfg.Listen, Handler: service.HTTP(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 90 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	maintenance := make(chan struct{})
	go func() { defer close(maintenance); service.MaintainCredentials(ctx) }()
	failure := make(chan error, 1)
	log.Info("social-mcp starting", "listen", cfg.Listen)
	go func() { failure <- server.Serve(listener) }()
	select {
	case err = <-failure:
		stop()
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
		_ = server.Close()
	}
	<-done
	<-maintenance
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
