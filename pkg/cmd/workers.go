package cmd

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/knz/shakespeare/pkg/crdb/caller"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"github.com/knz/shakespeare/pkg/crdb/syncutil"
)

type workerRegistry struct {
	syncutil.Mutex
	mu struct {
		numWorkers int
		workers    map[string]int
	}
}

func (r *workerRegistry) addWorker(name string) {
	r.Lock()
	defer r.Unlock()
	r.mu.workers[name]++
	r.mu.numWorkers++
}

func (r *workerRegistry) delWorker(name string) {
	r.Lock()
	defer r.Unlock()
	r.mu.workers[name]--
	r.mu.numWorkers--
}

var registry = func() *workerRegistry {
	w := workerRegistry{}
	w.mu.workers = make(map[string]int)
	return &w
}()

func (r *workerRegistry) String() string {
	r.Lock()
	defer r.Unlock()
	var buf bytes.Buffer
	if r.mu.numWorkers == 0 {
		buf.WriteString("(no running worker)")
	} else {
		fmt.Fprintf(&buf, "%d running workers:\n", r.mu.numWorkers)
		comma := ""
		for w, cnt := range r.mu.workers {
			if cnt == 0 {
				continue
			}
			buf.WriteString(comma)
			comma = "\n"
			fmt.Fprintf(&buf, "%-6d %s", cnt, w)
		}
	}
	return buf.String()
}

func showRunning(stopper *stop.Stopper) string {
	var buf bytes.Buffer
	buf.WriteString("currently running:\n")
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
	if log.V(1) {
		log.Info(ctx, "adding worker")
	}
	registry.addWorker(fullName)
	stopper.RunWorker(ctx, func(ctx context.Context) {
		w(ctx)
		if log.V(1) {
			log.Info(ctx, "removing worker")
		}
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
	if log.V(1) {
		log.Info(ctx, "adding task")
	}
	return stopper.RunAsyncTask(ctx, fullName, func(ctx context.Context) {
		w(ctx)
		if log.V(1) {
			log.Info(ctx, "removing task")
		}
	})
}
