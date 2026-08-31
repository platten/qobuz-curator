// Package privilege prevents the application from running with unnecessary
// operating-system privileges.
package privilege

import "fmt"

var elevated = isElevated

// RefuseElevated returns an error when the process is root or has an elevated
// Windows token. Qobuz Curator never needs administrative privileges.
func RefuseElevated() error {
	value, err := elevated()
	if err != nil {
		return fmt.Errorf("determine process privileges: %w", err)
	}
	if value {
		return fmt.Errorf("refusing to run as root or Administrator; use an unprivileged account")
	}
	return nil
}
