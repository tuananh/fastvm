package main

import (
	"os"
	"testing"
)

func TestEnsurePATHSetsDefaultWhenMissing(t *testing.T) {
	old, had := os.LookupEnv("PATH")
	defer func() {
		if had {
			_ = os.Setenv("PATH", old)
		} else {
			_ = os.Unsetenv("PATH")
		}
	}()

	if err := os.Unsetenv("PATH"); err != nil {
		t.Fatal(err)
	}
	ensurePATH()
	if got := os.Getenv("PATH"); got != defaultPath {
		t.Errorf("PATH = %q, want %q", got, defaultPath)
	}
}

func TestEnsurePATHKeepsExisting(t *testing.T) {
	old, had := os.LookupEnv("PATH")
	defer func() {
		if had {
			_ = os.Setenv("PATH", old)
		} else {
			_ = os.Unsetenv("PATH")
		}
	}()

	const custom = "/custom/bin"
	if err := os.Setenv("PATH", custom); err != nil {
		t.Fatal(err)
	}
	ensurePATH()
	if got := os.Getenv("PATH"); got != custom {
		t.Errorf("PATH = %q, want %q", got, custom)
	}
}
