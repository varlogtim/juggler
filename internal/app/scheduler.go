package app

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/varlogtim/juggler/internal/source"
)

// Scheduler pulls every source on its own interval. It lives in the
// long-running process (`jug serve`); one-shot runs go through Sync
// directly. Runs of different sources are serialized by Sync itself.
type Scheduler struct {
	app *App
	// OnReport is called after every run (the web layer uses it to push a
	// change event to browsers).
	OnReport func(*Report, error)
	Log      *log.Logger

	mu    sync.Mutex
	runs  map[string]*run
	wake  map[string]chan struct{}
	start time.Time
}

// run is what the scheduler knows about one source between pulls.
type run struct {
	running bool
	next    time.Time
}

// NewScheduler creates a scheduler for a; it is also registered on a so
// SourceStatuses can report running/next.
func NewScheduler(a *App, logger *log.Logger) *Scheduler {
	s := &Scheduler{app: a, Log: logger, runs: map[string]*run{}, wake: map[string]chan struct{}{}}
	a.Scheduler = s
	return s
}

// Run blocks until ctx ends. Each source with a non-zero poll gets its own
// loop: a first pull shortly after start, then one every Poll().
func (s *Scheduler) Run(ctx context.Context) {
	s.start = time.Now()
	srcs, errs := s.app.Sources()
	for name, err := range errs {
		if s.Log != nil {
			s.Log.Printf("source %s: not scheduled: %v", name, err)
		}
	}
	var wg sync.WaitGroup
	for _, src := range srcs {
		if src.Poll() <= 0 {
			if s.Log != nil {
				s.Log.Printf("source %s: poll 0, manual only", src.Name())
			}
			continue
		}
		wg.Add(1)
		go func(src source.Source) {
			defer wg.Done()
			s.loop(ctx, src)
		}(src)
	}
	wg.Wait()
}

// loop is one source's goroutine: pull, sleep until the next tick or a
// Wake, repeat until ctx ends. The first pull waits a few seconds so a
// freshly started service is answering requests before it talks to Jira.
func (s *Scheduler) loop(ctx context.Context, src source.Source) {
	wake := make(chan struct{}, 1)
	s.mu.Lock()
	s.wake[src.Name()] = wake
	s.runs[src.Name()] = &run{next: time.Now().Add(5 * time.Second)}
	s.mu.Unlock()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		s.pull(ctx, src)
		s.mu.Lock()
		s.runs[src.Name()].next = time.Now().Add(src.Poll())
		s.mu.Unlock()
		timer.Reset(src.Poll())
	}
}

// pull runs one sync for src, marking it running for SourceStatuses and
// logging the outcome; errors are recorded in the ledger by Sync itself.
func (s *Scheduler) pull(ctx context.Context, src source.Source) {
	s.mu.Lock()
	r := s.runs[src.Name()]
	if r == nil {
		r = &run{}
		s.runs[src.Name()] = r
	}
	r.running = true
	s.mu.Unlock()
	rep, err := s.app.Sync(ctx, src, SyncOptions{})
	s.mu.Lock()
	r.running = false
	s.mu.Unlock()
	if s.Log != nil {
		if err != nil {
			s.Log.Printf("sync %s: %v", src.Name(), err)
		} else {
			s.Log.Print("sync " + rep.Summary())
		}
	}
	if s.OnReport != nil {
		s.OnReport(rep, err)
	}
}

// Kick asks the loop for src to run now (no-op for unscheduled sources).
func (s *Scheduler) Kick(name string) bool {
	s.mu.Lock()
	ch := s.wake[name]
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- struct{}{}:
	default:
	}
	return true
}

// state reports running/next for SourceStatuses.
func (s *Scheduler) state(name string) (bool, *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[name]
	if r == nil {
		return false, nil
	}
	if r.next.IsZero() {
		return r.running, nil
	}
	n := r.next
	return r.running, &n
}
