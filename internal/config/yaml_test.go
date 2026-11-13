package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func mustNode(t *testing.T, raw string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal node: %v", err)
	}
	return n.Content[0]
}
