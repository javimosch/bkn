package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrLockHeld means somebody else holds the lock and it has not expired.
var ErrLockHeld = errors.New("lock is held by another owner")

// Lock is a held mutual-exclusion lease.
type Lock struct {
	Key        string `json:"key"`
	Owner      string `json:"owner"`
	ExpiresAt  string `json:"expires_at"`
	AcquiredAt string `json:"acquired_at"`
}

// Locks provides expiring mutual exclusion across processes.
//
// The in-process guard in the scheduler stops one daemon running a job twice;
// it does nothing about a CLI `cron tick` racing that daemon, or about two
// deployments sharing a database. Anything that must not run concurrently
// needs a lease that lives in the database, with an expiry so a crashed holder
// does not block the work forever.
type Locks struct{ db *sql.DB }

func NewLocks(db *sql.DB) *Locks { return &Locks{db: db} }

// Acquire takes the lock for ttl, or returns ErrLockHeld.
//
// The whole decision is one statement: insert, and on conflict take it over
// only if the existing lease has expired. Read-then-write would let two
// callers both observe an expired lock and both claim it.
func (l *Locks) Acquire(key string, ttl time.Duration) (Lock, error) {
	if ttl <= 0 {
		ttl = time.Minute
	}
	now := time.Now().UTC()
	lock := Lock{
		Key:        key,
		Owner:      NewID(),
		ExpiresAt:  now.Add(ttl).Format(time.RFC3339),
		AcquiredAt: now.Format(time.RFC3339),
	}
	res, err := l.db.Exec(`
		INSERT INTO locks (key, owner, expires_at, acquired_at) VALUES (?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
		  owner = excluded.owner,
		  expires_at = excluded.expires_at,
		  acquired_at = excluded.acquired_at
		WHERE locks.expires_at <= ?`,
		lock.Key, lock.Owner, lock.ExpiresAt, lock.AcquiredAt, now.Format(time.RFC3339))
	if err != nil {
		return Lock{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Lock{}, ErrLockHeld
	}
	return lock, nil
}

// Release drops the lock, but only if this owner still holds it. Releasing a
// lease that already expired and was taken over by someone else must not steal
// it from them.
func (l *Locks) Release(key, owner string) (bool, error) {
	res, err := l.db.Exec(`DELETE FROM locks WHERE key = ? AND owner = ?`, key, owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Renew extends a lease the caller still holds, for long jobs that would
// otherwise let it lapse mid-run.
func (l *Locks) Renew(key, owner string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = time.Minute
	}
	res, err := l.db.Exec(`
		UPDATE locks SET expires_at = ? WHERE key = ? AND owner = ? AND expires_at > ?`,
		time.Now().UTC().Add(ttl).Format(time.RFC3339), key, owner,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// List returns every lock, including expired ones that nobody has cleaned up.
func (l *Locks) List() ([]Lock, error) {
	rows, err := l.db.Query(`SELECT key, owner, expires_at, acquired_at FROM locks ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Lock{}
	for rows.Next() {
		var lock Lock
		if err := rows.Scan(&lock.Key, &lock.Owner, &lock.ExpiresAt, &lock.AcquiredAt); err != nil {
			return nil, err
		}
		out = append(out, lock)
	}
	return out, rows.Err()
}

// ForceRelease drops a lock regardless of owner: the operator's override for a
// lease whose holder is never coming back.
func (l *Locks) ForceRelease(key string) (bool, error) {
	res, err := l.db.Exec(`DELETE FROM locks WHERE key = ?`, key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
