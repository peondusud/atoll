package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"strings"
	"syscall"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/view/readonly"
)

// runMount serves Atoll's own view, read only, at a point beside mergerfs, until SIGTERM or SIGINT (specs.md §21.2).
func runMount(configPath string, args []string, stderr io.Writer) (code int) {
	point := ""
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-"):
			return usage(stderr, fmt.Sprintf(T("mount: unknown argument %q"), a))
		case point != "":
			return usage(stderr, T("mount: one point at most"))
		default:
			point = a
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if point == "" {
		if point, _ = c.Effective()["view.readonly_mount"].(string); point == "" {
			return usage(stderr, T("mount: no point: give one, or set view.readonly_mount"))
		}
	} else if err := config.CheckReadonlyMount(c, point); err != nil {
		return usage(stderr, "mount: "+err.Error())
	}
	machine, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	db, err := catalog.OpenPool(context.Background(), catalogPath(c), c.PoolID)
	if err != nil {
		return failure(stderr, err)
	}
	defer func() {
		if err := db.Close(); err != nil && code == exitOK {
			code = failure(stderr, err)
		}
	}()
	// A signal that comes before the view is mounted unmounts it at once.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log := slog.New(slog.NewTextHandler(stderr, nil))
	// What the view logs of a read, through the default logger, goes the same way.
	slog.SetDefault(log)
	ready := func() {
		if err := daemon.Notify("READY=1"); err != nil {
			log.Warn("systemd was not told the view is ready", "error", err)
		}
	}
	if err := readonly.Run(ctx, readonly.Options{Config: c, DB: db, Machine: machine, Point: point, Ready: ready, Log: log}); err != nil {
		return failure(stderr, err)
	}
	return exitOK
}
