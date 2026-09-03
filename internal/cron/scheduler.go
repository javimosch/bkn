package cron

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/javimosch/bkn/internal/events"
	"github.com/javimosch/bkn/internal/script"
)

// EventStream is where the scheduler records what it did.
const EventStream = "cron"

// Result is the outcome of one scheduled execution.
type Result struct {
	Job    string `json:"job"`
	Script string `json:"script"`
	Status string `json:"status"`
	RunID  string `json:"run_id,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Scheduler runs due jobs. It owns no timing state of its own: everything it
// needs is in the database, so a tick from the daemon and a tick from the CLI
// are the same operation.
type Scheduler struct {
	reg    *Registry
	runner *script.Runner
	log    *events.Log

	mu      sync.Mutex
	running map[string]bool
}

func NewScheduler(reg *Registry, runner *script.Runner, log *events.Log) *Scheduler {
	return &Scheduler{reg: reg, runner: runner, log: log, running: map[string]bool{}}
}

// Tick runs every job that is due, and returns what it did.
func (s *Scheduler) Tick(at time.Time) ([]Result, error) {
	jobs, err := s.reg.due(at)
	if err != nil {
		return nil, err
	}
	results := []Result{}
	for _, j := range jobs {
		schedule, err := Parse(j.Schedule)
		if err != nil {
			// A job whose schedule stopped parsing must not spin: disable it
			// and say so, rather than being retried on every tick forever.
			results = append(results, s.disableBroken(j, err))
			continue
		}
		claimed, err := s.reg.claim(j, schedule.Next(at))
		if err != nil {
			return results, err
		}
		if !claimed {
			continue // another ticker got there first
		}
		if r, ok := s.run(j); ok {
			results = append(results, r)
		}
	}
	return results, nil
}

func (s *Scheduler) disableBroken(j Job, cause error) Result {
	_ = s.reg.disable(j.Name)
	_ = s.reg.recordRun(j.Name, "invalid", "", time.Now())
	s.emit(events.LevelError, "cron.invalid", j.Name, map[string]any{
		"schedule": j.Schedule, "error": cause.Error(),
	})
	return Result{Job: j.Name, Script: j.Script, Status: "invalid", Error: cause.Error()}
}

// run executes one job, skipping it if the previous execution is still going.
func (s *Scheduler) run(j Job) (Result, bool) {
	s.mu.Lock()
	if s.running[j.Name] {
		s.mu.Unlock()
		// A job that takes longer than its interval would otherwise pile up
		// copies of itself until something falls over.
		s.emit(events.LevelWarn, "cron.overlap", j.Name, map[string]any{"script": j.Script})
		return Result{Job: j.Name, Script: j.Script, Status: "skipped",
			Error: "the previous run has not finished"}, true
	}
	s.running[j.Name] = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.running, j.Name)
		s.mu.Unlock()
	}()

	started := time.Now()
	res, err := s.runner.Run(j.Script, j.Input)
	if err != nil {
		_ = s.reg.recordRun(j.Name, "error", "", started)
		s.emit(events.LevelError, "cron.failed", j.Name, map[string]any{
			"script": j.Script, "error": err.Error(),
		})
		return Result{Job: j.Name, Script: j.Script, Status: "error", Error: err.Error()}, true
	}

	_ = s.reg.recordRun(j.Name, res.Run.Status, res.Run.ID, started)
	level := events.LevelInfo
	eventType := "cron.ok"
	if !res.OK {
		level, eventType = events.LevelError, "cron.failed"
	}
	s.emit(level, eventType, j.Name, map[string]any{
		"script": j.Script, "run_id": res.Run.ID,
		"duration_ms": res.Run.DurationMS, "error": res.Run.Error,
	})
	return Result{Job: j.Name, Script: j.Script, Status: res.Run.Status,
		RunID: res.Run.ID, Error: res.Run.Error}, true
}

// RunNow executes a job immediately without touching its schedule.
func (s *Scheduler) RunNow(name string) (Result, error) {
	j, err := s.reg.Get(name)
	if err != nil {
		return Result{}, err
	}
	r, _ := s.run(j)
	return r, nil
}

func (s *Scheduler) emit(level, eventType, job string, data map[string]any) {
	if s.log == nil {
		return
	}
	if _, err := s.log.Emit(events.Event{
		Stream: EventStream, Type: eventType, Level: level,
		Source: "cron", Subject: job, Data: data,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[cron] could not record %s for %s: %v\n", eventType, job, err)
	}
}

// TickInterval is how often the daemon looks for due work. Cron has minute
// resolution, so this only has to be comfortably under a minute.
const TickInterval = 20 * time.Second

// Start ticks until the context is cancelled. It runs inside `bkn serve`;
// without a running daemon nothing fires on its own, which is why `bkn cron
// tick` exists for systemd timers and the like.
func (s *Scheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			results, err := s.Tick(at)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[cron] tick failed: %v\n", err)
				continue
			}
			for _, r := range results {
				fmt.Fprintf(os.Stderr, "[cron] %s -> %s (%s)\n", r.Job, r.Status, r.Script)
			}
		}
	}
}
