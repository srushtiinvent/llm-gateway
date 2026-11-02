package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/store"
)

// logWriter saves request logs in the background, so writing to the database never
// slows down an answer. If the buffer is full (database very slow) logs are dropped
// and counted, rather than making users wait.
type logWriter struct {
	store   store.Store
	logger  *slog.Logger
	ch      chan store.RequestLog
	done    chan struct{}
	dropped atomic.Int64
	once    sync.Once
}

func newLogWriter(s store.Store, lg *slog.Logger) *logWriter {
	w := &logWriter{store: s, logger: lg, ch: make(chan store.RequestLog, 2000), done: make(chan struct{})}
	go w.run()
	return w
}

func (w *logWriter) add(l store.RequestLog) {
	defer func() { _ = recover() }() // a send after shutdown began would panic; just ignore it
	if l.TS.IsZero() {
		l.TS = time.Now()
	}
	select {
	case w.ch <- l:
	default:
		if n := w.dropped.Add(1); n == 1 || n%100 == 0 {
			w.logger.Warn("request log buffer full, dropping logs", "dropped_total", n)
		}
	}
}

func (w *logWriter) run() {
	defer close(w.done)
	for l := range w.ch {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := w.store.InsertLog(ctx, l); err != nil {
			w.logger.Warn("could not save request log", "err", err)
		}
		cancel()
	}
}

// close stops accepting logs and waits (briefly) for the queue to drain.
func (w *logWriter) close() {
	w.once.Do(func() {
		close(w.ch)
		select {
		case <-w.done:
		case <-time.After(10 * time.Second):
		}
	})
}
