//go:build !windows

package tun

// SetupFirewall is a no-op on non-Windows platforms.
func SetupFirewall() (cleanup func(), err error) {
	return func() {}, nil
}
