package auth

import "database/sql"

// DB exposes the connection to the package's external test so it can plant a
// row that looks like one migrated from the Node backend.
func (a *Auth) DB() *sql.DB { return a.db }
