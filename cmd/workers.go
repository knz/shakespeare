package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/knz/shakespeare/cmd/caller"
	"github.com/knz/shakespeare/cmd/log"
	"github.com/knz/shakespeare/cmd/stop"
	"github.com/knz/shakespeare/cmd/syncutil"
)

type workerRegistry struct {
	syncutil.Mutex
	mu struct {
		workers map[string]struct{}
	}
}

func (r *workerRegistry) addWorker(name string) {
	r.Lock()
	defer r.Unlock()
	r.mu.workers[name] = struct{}{}
}

func (r *workerRegistry) delWorker(name string) {
	r.Lock()
	defer r.Unlock()
	delete(r.mu.workers, name)
}

var registry = func() *workerRegistry {
	w := workerRegistry{}
	w.mu.workers = make(map[string]struct{})
	return &w
}()

func (r *workerRegistry) String() string {
	r.Lock()
	defer r.Unlock()
	var buf bytes.Buffer
	if len(r.mu.workers) == 0 {
		buf.WriteString("(no running worker)")
	} else {
		fmt.Fprintf(&buf, "%d running workers:\n", len(r.mu.workers))
		comma := ""
		for w := range r.mu.workers {
			buf.WriteString(comma)
			comma = "\n"
			fmt.Fprint(&buf, "  ", w)
		}
	}
	return buf.String()
}

func showRunning(stopper *stop.Stopper) string {
	var buf bytes.Buffer
	fmt.Fprintln(&buf, registry.String())
	tasks := stopper.RunningTasks()
	if len(tasks) == 0 {
		fmt.Fprint(&buf, "(no running task)")
	} else {
		fmt.Fprintf(&buf, "%d running tasks:\n%s", len(tasks), tasks.String())
	}
	return buf.String()
}

func runWorker(ctx context.Context, stopper *stop.Stopper, w func(context.Context)) {
	fullName := getTaskName(1, ctx)
	log.Info(ctx, "adding worker")
	registry.addWorker(fullName)
	stopper.RunWorker(ctx, func(ctx context.Context) {
		w(ctx)
		log.Info(ctx, "removing worker")
		registry.delWorker(fullName)
	})
}

func getTaskName(depth int, ctx context.Context) string {
	var buf strings.Builder
	f, l, _ := caller.Lookup(depth + 1)
	f = filepath.Base(f)
	fmt.Fprintf(&buf, "%s:%d ", f, l)
	log.FormatTags(ctx, &buf)
	return buf.String()
}

func runAsyncTask(ctx context.Context, stopper *stop.Stopper, w func(ctx context.Context)) error {
	fullName := getTaskName(1, ctx)
	log.Info(ctx, "adding task")
	return stopper.RunAsyncTask(ctx, fullName, func(ctx context.Context) {
		w(ctx)
		log.Info(ctx, "removing task")
	})
}
