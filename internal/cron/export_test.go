package cron

// SetScheduleForTest writes a schedule without validating it, so the external
// test can reproduce a row corrupted by a bad migration or a hand edit.
func (r *Registry) SetScheduleForTest(name, schedule string) error {
	_, err := r.db.Exec(`UPDATE cron_jobs SET schedule = ? WHERE name = ?`, schedule, name)
	return err
}
