package credentials

import (
	"errors"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

func TestSystemStoreWithMockKeyring(t *testing.T) {
	keyring.MockInit()
	store := SystemStore{}
	if _, err := store.Get(QobuzToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing error = %v", err)
	}
	if err := store.Set(QobuzToken, "secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(QobuzToken); err != nil || got != "secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := store.Delete(QobuzToken); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(OpenAIAPIKey, ""); err == nil {
		t.Fatal("expected empty credential rejection")
	}
	keyring.MockInitWithError(errors.New("vault unavailable"))
	if _, err := store.Get(QobuzToken); err == nil || !strings.Contains(err.Error(), "vault unavailable") {
		t.Fatalf("Get error = %v", err)
	}
	if err := store.Set(QobuzToken, "secret"); err == nil || !strings.Contains(err.Error(), "vault unavailable") {
		t.Fatalf("Set error = %v", err)
	}
	if err := store.Delete(QobuzToken); err == nil || !strings.Contains(err.Error(), "vault unavailable") {
		t.Fatalf("Delete error = %v", err)
	}
}
