//go:build windows

package privilege

import "golang.org/x/sys/windows"

func isElevated() (bool, error) {
	token := windows.GetCurrentProcessToken()
	return token.IsElevated(), nil
}
