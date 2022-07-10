package graterm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"time"
)

// Terminator is a component terminator that executes registered termination hooks in a specified order.
type Terminator struct {
	hooksMx *sync.Mutex
	hooks   map[Order][]Hook

	wg *sync.WaitGroup

	cancelFunc context.CancelFunc

	log Logger
}

// NewWithSignals creates a new instance of component Terminator.
//
// Example of useful signals might be: [syscall.SIGINT], [syscall.SIGTERM].
//
// Note: this method will start internal monitoring goroutine.
func NewWithSignals(appCtx context.Context, sig ...os.Signal) (*Terminator, context.Context) {
	chSignals := make(chan os.Signal, 1)
	ctx, cancel := withSignals(appCtx, chSignals, sig...)
	return &Terminator{
		hooksMx:    &sync.Mutex{},
		hooks:      make(map[Order][]Hook),
		wg:         &sync.WaitGroup{},
		cancelFunc: cancel,
		log:        noopLogger{},
	}, ctx
}

// withSignals return a copy of the parent context that will be canceled by signal(s).
// If no signals are provided, any incoming signal will cause cancel.
// Otherwise, just the provided signals will.
//
// Note: this method will start internal monitoring goroutine.
func withSignals(ctx context.Context, chSignals chan os.Signal, sig ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)

	signal.Notify(chSignals, sig...)

	// function invoke cancel once a signal arrived OR parent context is done:
	go func() {
		defer cancel()

		select {
		case <-chSignals:
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}

// SetLogger sets the logger implementation.
//
// If log is nil, then NOOP logger implementation will be used.
func (t *Terminator) SetLogger(log Logger) {
	if log == nil {
		log = noopLogger{}
	}

	t.hooksMx.Lock()
	defer t.hooksMx.Unlock()
	t.log = log
}

// WithOrder sets the Order for the termination hook.
// It starts registration chain to register termination hook with priority.
//
// The lower the order the higher the execution priority, the earlier it will be executed.
// If there are multiple hooks with the same order they will be executed in parallel.
func (t *Terminator) WithOrder(order Order) *Hook {
	return &Hook{
		terminator: t,
		order:      order,
	}
}

// Wait waits (with timeout) for Terminator to finish termination after the appCtx is done.
func (t *Terminator) Wait(appCtx context.Context, timeout time.Duration) error {
	{
		t.wg.Add(1)
		go t.waitShutdown(appCtx)
	}

	<-appCtx.Done()

	wgChan := make(chan struct{})
	go func() {
		defer close(wgChan)
		t.wg.Wait()
	}()

	select {
	case <-time.After(timeout):
		return fmt.Errorf("termination timed out after %v", timeout)
	case <-wgChan:
		return nil
	}
}

// waitShutdown waits for the context to be done and then sequentially notifies existing shutdown hooks.
func (t *Terminator) waitShutdown(appCtx context.Context) {
	defer t.wg.Done()

	<-appCtx.Done() // Block until application context is done (most likely, when the registered os.Signal will be received)

	t.hooksMx.Lock()
	defer t.hooksMx.Unlock()

	order := make([]int, 0, len(t.hooks))
	for k := range t.hooks {
		order = append(order, int(k))
	}
	sort.Ints(order)

	for _, o := range order {
		o := o

		runWg := sync.WaitGroup{}

		for _, c := range t.hooks[Order(o)] {
			runWg.Add(1)

			go func(f Hook) {
				defer runWg.Done()

				ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
				defer cancel()

				go func() {
					defer func() {
						defer cancel()

						if err := recover(); err != nil {
							t.log.Printf("registered hook panicked for %v, recovered: %+v", &f, err)
						}
					}()

					f.hookFunc(ctx)
				}()

				<-ctx.Done() // block until the hookFunc is over OR timeout has been expired

				switch err := ctx.Err(); {
				case errors.Is(err, context.DeadlineExceeded):
					t.log.Printf("registered hook timed out for %v", &f)
				case errors.Is(err, context.Canceled):
					t.log.Printf("registered hook finished termination in time for %v", &f)
				}
			}(c)
		}

		runWg.Wait()
	}
}
