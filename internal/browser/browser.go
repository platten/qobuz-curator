// Package browser opens URLs with the platform's default browser.
package browser

import (
	"fmt"
	"os/exec"
	"runtime"
)

var run = func(name string, args ...string) error { return exec.Command(name, args...).Start() }

func command(goos, url string) (string, []string, error) {
	switch goos {
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}, nil
	case "darwin":
		return "open", []string{url}, nil
	case "linux":
		return "xdg-open", []string{url}, nil
	default:
		return "", nil, fmt.Errorf("opening a browser is unsupported on %s", goos)
	}
}

func Open(url string) error {
	name, args, err := command(runtime.GOOS, url)
	if err != nil {
		return err
	}
	return run(name, args...)
}
