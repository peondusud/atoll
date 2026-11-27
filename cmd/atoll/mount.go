package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/telemetry"
	"github.com/peondusud/atoll/internal/view/readonly"
)

// mountPoint is a point atoll mount serves the view at, and whether the view writes there.
type mountPoint struct {
	path   string
	writes bool
}

// runMount serves Atoll's own view until SIGTERM or SIGINT (specs.md §21.2): read only at a point given, and otherwise
// at view.mount, where it writes, with view.engine = "atoll", and read only at view.readonly_mount when it is set.
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
	var points []mountPoint
	if point == "" {
		if engine, _ := c.Effective()["view.engine"].(string); engine == "atoll" {
			mount, _ := c.Effective()["view.mount"].(string)
			points = append(points, mountPoint{path: mount, writes: true})
		}
		if readonly, _ := c.Effective()["view.readonly_mount"].(string); readonly != "" {
			points = append(points, mountPoint{path: readonly})
		}
		if len(points) == 0 {
			return usage(stderr, T("mount: no point: give one, set view.readonly_mount, or view.engine = \"atoll\""))
		}
	} else if err := config.CheckReadonlyMount(c, point); err != nil {
		return usage(stderr, "mount: "+err.Error())
	} else {
		points = append(points, mountPoint{path: point})
	}
	machine, err := newMachine()
	if err != nil {
		return failure(stderr, err)
	}
	// Under the view's side of the lock, for as long as the view runs: no rebuild sets its catalog aside meanwhile, and
	// none runs as it opens it (specs.md §21.2).
	db, release, err := openView(context.Background(), c)
	if err != nil {
		return failure(stderr, err)
	}
	defer release()
	defer func() {
		if err := db.Close(); err != nil && code == exitOK {
			code = failure(stderr, err)
		}
	}()
	// A signal that comes before the view is mounted unmounts it at once.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	logs := logsFor(stderr)
	log := slog.New(logs)
	// What the view logs of a read, through the default logger, goes the same way, and over OTLP when [telemetry] asks:
	// the view has no metrics (specs.md §23.2).
	slog.SetDefault(log)
	tel := telemetry.NewManager(telemetry.Options{Logs: logs, Version: atollVersion()})
	applyTelemetry(ctx, tel, c, log)
	defer stopTelemetry(tel, log)
	// systemd is told once every point is mounted; one that fails or is unmounted stops the others.
	var mounted atomic.Int32
	ready := func() {
		if int(mounted.Add(1)) < len(points) {
			return
		}
		if err := daemon.Notify("READY=1"); err != nil {
			log.Warn("systemd was not told the view is ready", "error", err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ended := make(chan error, len(points))
	for _, p := range points {
		go func() {
			err := readonly.Run(ctx, readonly.Options{Config: c, DB: db, Machine: machine, Point: p.path, Writes: p.writes, Ready: ready, Log: log})
			if err != nil {
				cancel()
			}
			ended <- err
		}()
	}
	var failed error
	for range points {
		if err := <-ended; err != nil && failed == nil {
			failed = err
		}
	}
	if failed != nil {
		return failure(stderr, failed)
	}
	return exitOK
}
