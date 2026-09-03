package store_test

import (
	"sync"
	"testing"
	"time"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/store"
)

func newLocks(t *testing.T) *store.Locks {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return store.NewLocks(conn)
}

func TestLockExcludesAndReleases(t *testing.T) {
	l := newLocks(t)

	held, err := l.Acquire("job", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := l.Acquire("job", time.Minute); err != store.ErrLockHeld {
		t.Errorf("second Acquire = %v, want ErrLockHeld", err)
	}
	// A different key is unaffected.
	if _, err := l.Acquire("other", time.Minute); err != nil {
		t.Errorf("Acquire on a different key: %v", err)
	}

	released, err := l.Release("job", held.Owner)
	if err != nil || !released {
		t.Fatalf("Release = %v, %v", released, err)
	}
	if _, err := l.Acquire("job", time.Minute); err != nil {
		t.Errorf("Acquire after release: %v", err)
	}
}

// A crashed holder must not block the work forever.
func TestExpiredLockIsTakenOver(t *testing.T) {
	l := newLocks(t)
	first, err := l.Acquire("job", 1*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := l.Acquire("job", time.Minute); err != store.ErrLockHeld {
		t.Fatalf("takeover before expiry = %v, want ErrLockHeld", err)
	}

	time.Sleep(1100 * time.Millisecond)
	second, err := l.Acquire("job", time.Minute)
	if err != nil {
		t.Fatalf("takeover after expiry: %v", err)
	}
	if second.Owner == first.Owner {
		t.Error("takeover reused the previous owner token")
	}
	// The original holder must not be able to release a lease that has since
	// been taken over by somebody else.
	released, err := l.Release("job", first.Owner)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if released {
		t.Error("a stale owner released somebody else's lock")
	}
}

// The whole point of the lock is that a read-then-write race cannot produce
// two holders.
func TestConcurrentAcquireYieldsExactlyOneWinner(t *testing.T) {
	l := newLocks(t)

	const attempts = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			if _, err := l.Acquire("contended", time.Minute); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Errorf("%d goroutines acquired the same lock, want exactly 1", winners)
	}
}

func TestRenewOnlyWorksForTheHolder(t *testing.T) {
	l := newLocks(t)
	held, err := l.Acquire("job", 2*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ok, err := l.Renew("job", held.Owner, time.Hour)
	if err != nil || !ok {
		t.Fatalf("Renew by the holder = %v, %v", ok, err)
	}
	ok, err = l.Renew("job", "someone-else", time.Hour)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if ok {
		t.Error("a non-holder renewed the lock")
	}
	// The renewal took, so the lease has not lapsed.
	if _, err := l.Acquire("job", time.Minute); err != store.ErrLockHeld {
		t.Errorf("Acquire after renew = %v, want ErrLockHeld", err)
	}
}

func TestForceReleaseIsTheOperatorOverride(t *testing.T) {
	l := newLocks(t)
	if _, err := l.Acquire("stuck", time.Hour); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	released, err := l.ForceRelease("stuck")
	if err != nil || !released {
		t.Fatalf("ForceRelease = %v, %v", released, err)
	}
	if _, err := l.Acquire("stuck", time.Minute); err != nil {
		t.Errorf("Acquire after force release: %v", err)
	}
	list, err := l.List()
	if err != nil || len(list) != 1 {
		t.Errorf("List = %d locks, %v", len(list), err)
	}
}

// PutIfAbsent is the idempotency primitive: the second writer must be told it
// lost, and must not overwrite what the first one stored.
func TestPutIfAbsent(t *testing.T) {
	s := newStore(t)
	ref := ref(t, "webhooks/events")

	rec, created, err := s.PutIfAbsent(ref, "evt_1", map[string]any{"attempt": 1})
	if err != nil || !created {
		t.Fatalf("first PutIfAbsent = %v, created=%v, %v", rec, created, err)
	}
	existing, created, err := s.PutIfAbsent(ref, "evt_1", map[string]any{"attempt": 2})
	if err != nil {
		t.Fatalf("second PutIfAbsent: %v", err)
	}
	if created {
		t.Error("PutIfAbsent overwrote an existing record")
	}
	if existing["attempt"] != float64(1) {
		t.Errorf("existing record = %v, want the original attempt 1", existing)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	wg.Add(16)
	for i := 0; i < 16; i++ {
		go func() {
			defer wg.Done()
			if _, ok, err := s.PutIfAbsent(ref, "evt_race", map[string]any{"x": 1}); err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent writers created the same id, want 1", wins)
	}
}
