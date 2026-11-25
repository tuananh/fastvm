package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExamplesLoad(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test")
	t.Setenv("OPENAI_API_KEY", "test")
	t.Setenv("ANTHROPIC_API_KEY", "test")
	t.Setenv("GITHUB_TOKEN", "test")
	t.Setenv("RUNNER_TOKEN", "test")
	t.Setenv("REPO_URL", "https://github.com/example/repo")

	root := filepath.Join("..", "..", "examples")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".fvmfile" {
			return err
		}
		if _, err := LoadFVMFile(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
