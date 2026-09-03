package cron_test

import (
	"errors"
	"testing"
	"time"

	"github.com/javimosch/bkn/internal/cron"
	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/events"
	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/store"
)

func setup(t *testing.T) (*cron.Registry, *cron.Scheduler, *script.Registry, *events.Log) {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	scripts := script.NewRegistry(conn)
	log := events.New(conn)
	runner := script.NewRunner(scripts, store.New(conn), kv.New(conn, nil, 0), nil, nil, log)
	reg := cron.NewRegistry(conn)
	return reg, cron.NewScheduler(reg, runner, log), scripts, log
}

func mustScript(t *testing.T, scripts *script.Registry, name, code string) {
	t.Helper()
	if _, err := scripts.Create(script.Script{Name: name, Code: code}); err != nil {
		t.Fatalf("script.Create(%s): %v", name, err)
	}
}

func mustJob(t *testing.T, reg *cron.Registry, j cron.Job) cron.Job {
	t.Helper()
	out, err := reg.Create(j)
	if err != nil {
		t.Fatalf("cron.Create(%s): %v", j.Name, err)
	}
	return out
}

func TestCreateValidatesUpFront(t *testing.T) {
	reg, _, scripts, _ := setup(t)
	mustScript(t, scripts, "ping", "function main(){ return 1 }")

	j := mustJob(t, reg, cron.Job{Name: "ok", Schedule: "@daily", Script: "ping"})
	if j.NextRunAt == "" {
		t.Error("next_run_at was not computed at creation")
	}
	if !j.Enabled {
		t.Error("a new job should be enabled")
	}
	// An unparseable schedule must fail now, not silently never fire.
	if _, err := reg.Create(cron.Job{Name: "bad", Schedule: "nonsense", Script: "ping"}); !errors.Is(err, cron.ErrBadSchedule) {
		t.Errorf("bad schedule = %v, want ErrBadSchedule", err)
	}
	for _, bad := range []string{"Bad", "9lives", "has space", ""} {
		if _, err := reg.Create(cron.Job{Name: bad, Schedule: "@daily", Script: "ping"}); !errors.Is(err, cron.ErrBadName) {
			t.Errorf("Create(%q) = %v, want ErrBadName", bad, err)
		}
	}
	if _, err := reg.Create(cron.Job{Name: "ok", Schedule: "@daily", Script: "ping"}); !errors.Is(err, cron.ErrExists) {
		t.Error("a duplicate name was accepted")
	}
}

func TestTickRunsDueJobsAndAdvancesThem(t *testing.T) {
	reg, scheduler, scripts, log := setup(t)
	mustScript(t, scripts, "ping", "function main(){ return {ok:true} }")
	mustJob(t, reg, cron.Job{Name: "beat", Schedule: "@every 1s", Script: "ping"})

	// Nothing is due yet.
	results, err := scheduler.Tick(time.Now())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("ran %d jobs before anything was due", len(results))
	}

	time.Sleep(1100 * time.Millisecond)
	results, err = scheduler.Tick(time.Now())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(results) != 1 || results[0].Status != script.StatusOK {
		t.Fatalf("results = %+v, want one successful run", results)
	}

	// The job was claimed, so an immediate second tick must not re-run it.
	again, err := scheduler.Tick(time.Now())
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("a claimed job ran twice: %+v", again)
	}

	j, err := reg.Get("beat")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.LastStatus != script.StatusOK || j.LastRunID == "" {
		t.Errorf("job state = %+v, want the run recorded", j)
	}

	// The scheduler records what it did.
	logged, err := log.List(cron.EventStream, events.Query{})
	if err != nil || len(logged) != 1 || logged[0].Type != "cron.ok" {
		t.Errorf("cron events = %+v, %v", logged, err)
	}
}

func TestDisabledJobsDoNotRun(t *testing.T) {
	reg, scheduler, scripts, _ := setup(t)
	mustScript(t, scripts, "ping", "function main(){ return 1 }")
	mustJob(t, reg, cron.Job{Name: "beat", Schedule: "@every 1s", Script: "ping"})

	no := false
	j, err := reg.Update("beat", nil, nil, nil, &no)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if j.NextRunAt != "" {
		t.Error("a disabled job kept a next_run_at and would fire when re-enabled")
	}

	time.Sleep(1100 * time.Millisecond)
	results, err := scheduler.Tick(time.Now())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("a disabled job ran: %+v", results)
	}
}

// A failing script is a recorded failure, not a scheduler error.
func TestFailingScriptIsRecorded(t *testing.T) {
	reg, scheduler, scripts, log := setup(t)
	mustScript(t, scripts, "boom", `function main(){ throw new Error("nope") }`)
	mustJob(t, reg, cron.Job{Name: "breaks", Schedule: "@every 1s", Script: "boom"})

	time.Sleep(1100 * time.Millisecond)
	results, err := scheduler.Tick(time.Now())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(results) != 1 || results[0].Status == script.StatusOK {
		t.Fatalf("results = %+v, want a recorded failure", results)
	}
	logged, err := log.List(cron.EventStream, events.Query{Level: events.LevelError})
	if err != nil || len(logged) != 1 || logged[0].Type != "cron.failed" {
		t.Errorf("expected a cron.failed error event, got %+v (%v)", logged, err)
	}
}

// A job whose schedule stopped parsing must be disabled rather than retried on
// every tick forever.
func TestJobWithABrokenScheduleIsDisabled(t *testing.T) {
	reg, scheduler, scripts, _ := setup(t)
	mustScript(t, scripts, "ping", "function main(){ return 1 }")
	mustJob(t, reg, cron.Job{Name: "rotten", Schedule: "@every 1s", Script: "ping"})

	// Corrupt the stored schedule the way a bad migration or a hand edit would.
	if err := reg.SetScheduleForTest("rotten", "nonsense"); err != nil {
		t.Fatalf("SetScheduleForTest: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)

	results, err := scheduler.Tick(time.Now())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(results) != 1 || results[0].Status != "invalid" {
		t.Fatalf("results = %+v, want one invalid job", results)
	}
	j, err := reg.Get("rotten")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Enabled {
		t.Error("a job with an unparseable schedule stayed enabled")
	}
}

func TestRunNowIgnoresTheSchedule(t *testing.T) {
	reg, scheduler, scripts, _ := setup(t)
	mustScript(t, scripts, "ping", "function main(){ return 1 }")
	j := mustJob(t, reg, cron.Job{Name: "yearly", Schedule: "@yearly", Script: "ping"})

	res, err := scheduler.RunNow("yearly")
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if res.Status != script.StatusOK {
		t.Errorf("status = %q", res.Status)
	}
	after, err := reg.Get("yearly")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// A manual run must not consume the scheduled slot.
	if after.NextRunAt != j.NextRunAt {
		t.Errorf("next_run_at changed from %q to %q", j.NextRunAt, after.NextRunAt)
	}
	if _, err := scheduler.RunNow("absent"); !errors.Is(err, cron.ErrNotFound) {
		t.Errorf("RunNow on a missing job = %v, want ErrNotFound", err)
	}
}

func TestUpdateRecomputesTheNextRun(t *testing.T) {
	reg, _, scripts, _ := setup(t)
	mustScript(t, scripts, "ping", "function main(){ return 1 }")
	mustJob(t, reg, cron.Job{Name: "beat", Schedule: "@yearly", Script: "ping"})

	daily := "@daily"
	j, err := reg.Update("beat", &daily, nil, nil, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	next, err := time.Parse(time.RFC3339, j.NextRunAt)
	if err != nil {
		t.Fatalf("next_run_at %q: %v", j.NextRunAt, err)
	}
	if time.Until(next) > 25*time.Hour {
		t.Errorf("next_run_at = %s, want it recomputed for @daily", j.NextRunAt)
	}
	bad := "nonsense"
	if _, err := reg.Update("beat", &bad, nil, nil, nil); !errors.Is(err, cron.ErrBadSchedule) {
		t.Error("an invalid schedule was accepted by Update")
	}
}
