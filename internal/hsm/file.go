package hsm

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
)

// OpenLocalSigner persists the demo signing seed so that restarts retain verification ability.
// Publish the fully written file atomically without overwriting a concurrent creator.
func OpenLocalSigner(path string) (*SoftHSM, error) {
	seed, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		dir := filepath.Dir(path)
		if err = os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		f, err := os.CreateTemp(dir, ".signer-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		seed = make([]byte, ed25519.SeedSize)
		if _, err = rand.Read(seed); err == nil {
			_, err = f.Write(seed)
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err = os.Link(f.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		return OpenLocalSigner(path)
	}
	if err != nil {
		return nil, err
	}
	return NewSoftHSM("local-ed25519:v1", seed)
}
