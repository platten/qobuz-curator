//go:build !unix && !windows

package privilege

func isElevated() (bool, error) { return false, nil }
