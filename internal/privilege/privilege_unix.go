//go:build unix

package privilege

import "os"

func isElevated() (bool, error) { return os.Geteuid() == 0, nil }
