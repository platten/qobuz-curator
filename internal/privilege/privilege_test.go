package privilege

import (
	"errors"
	"strings"
	"testing"
)

func TestRefuseElevated(t *testing.T) {
	original := elevated
	t.Cleanup(func() { elevated = original })
	if _, err := original(); err != nil {
		t.Fatalf("platform privilege check failed: %v", err)
	}
	for _, test := range []struct {
		name      string
		elevated  bool
		checkErr  error
		wantError string
	}{
		{"ordinary user", false, nil, ""},
		{"administrator", true, nil, "refusing"},
		{"check failure", false, errors.New("token"), "determine"},
	} {
		t.Run(test.name, func(t *testing.T) {
			elevated = func() (bool, error) { return test.elevated, test.checkErr }
			err := RefuseElevated()
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatal(err)
			}
		})
	}
}
