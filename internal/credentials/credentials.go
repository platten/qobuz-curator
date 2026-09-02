// Package credentials provides the desktop application's OS-vault boundary.
package credentials

import (
	"errors"
	"fmt"

	keyring "github.com/zalando/go-keyring"
)

const (
	ServiceName  = "Qobuz Curator"
	QobuzToken   = "qobuz-user-auth-token"
	OpenAIAPIKey = "openai-api-key"
)

var ErrNotFound = errors.New("credential not found")

// Store is deliberately small so desktop setup can be tested without an OS vault.
type Store interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
}

// SystemStore uses Keychain, Windows Credential Manager, or Secret Service.
type SystemStore struct{}

func (SystemStore) Get(name string) (string, error) {
	value, err := keyring.Get(ServiceName, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read %s from the operating-system credential store: %w", name, err)
	}
	return value, nil
}

func (SystemStore) Set(name, value string) error {
	if value == "" {
		return fmt.Errorf("refuse to store an empty credential")
	}
	if err := keyring.Set(ServiceName, name, value); err != nil {
		return fmt.Errorf("save %s in the operating-system credential store: %w", name, err)
	}
	return nil
}

func (SystemStore) Delete(name string) error {
	err := keyring.Delete(ServiceName, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s from the operating-system credential store: %w", name, err)
	}
	return nil
}
