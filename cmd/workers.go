package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"

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
	buf.WriteString("running workers:\n")
	for w := range r.mu.workers {
		fmt.Fprintln(&buf, "  ", w)
	}
	return buf.String()
}

func showRunning(stopper *stop.Stopper) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s\nrunning tasks:\n%s",
		registry.String(), stopper.RunningTasks())
	return buf.String()
}

func runWorker(ctx context.Context, stopper *stop.Stopper, w func(context.Context)) {
	fullName := getTaskName(ctx)
	log.Info(ctx, "adding worker")
	registry.addWorker(fullName)
	stopper.RunWorker(ctx, func(ctx context.Context) {
		w(ctx)
		log.Info(ctx, "removing worker")
		registry.delWorker(fullName)
	})
}

func getTaskName(ctx context.Context) string {
	var buf strings.Builder
	log.FormatTags(ctx, &buf)
	return buf.String()
}

func runAsyncTask(ctx context.Context, stopper *stop.Stopper, w func(ctx context.Context)) error {
	fullName := getTaskName(ctx)
	log.Info(ctx, "adding task")
	return stopper.RunAsyncTask(ctx, fullName, func(ctx context.Context) {
		w(ctx)
		log.Info(ctx, "removing task")
	})
}
