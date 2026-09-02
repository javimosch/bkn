package script

// HostAllowedForTest exposes the allowlist matcher to the package's external
// test, which is where the security-relevant cases are documented.
func HostAllowedForTest(host string, allow []string) bool { return hostAllowed(host, allow) }
