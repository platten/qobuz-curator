package browser

import (
	"errors"
	"testing"
)

func TestOpen(t *testing.T) {
	old := run
	defer func() { run = old }()
	var name string
	run = func(n string, args ...string) error {
		name = n
		if len(args) == 0 {
			t.Fatal("missing args")
		}
		return nil
	}
	if e := Open("http://example.test"); e != nil || name == "" {
		t.Fatal(e)
	}
}
func TestCommands(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		if name, args, err := command(goos, "url"); err != nil || name == "" || len(args) == 0 {
			t.Fatal(goos, name, args, err)
		}
	}
	if _, _, err := command("plan9", "url"); err == nil {
		t.Fatal("unsupported")
	}
}

func TestOpenError(t *testing.T) {
	original := run
	defer func() { run = original }()
	run = func(string, ...string) error { return errors.New("open") }
	if err := Open("http://example.test"); err == nil {
		t.Fatal("expected browser error")
	}
}
