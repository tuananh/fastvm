package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListHasExpectedTemplates(t *testing.T) {
	seen := map[string]bool{}
	for _, tpl := range List() {
		if tpl.Name == "" || tpl.Agent == "" || tpl.Description == "" || tpl.Image == "" {
			t.Errorf("incomplete template: %+v", tpl)
		}
		if seen[tpl.Name] {
			t.Errorf("duplicate template %q", tpl.Name)
		}
		seen[tpl.Name] = true
		if !strings.HasPrefix(tpl.Image, "fastvm/"+tpl.Name+":") {
			t.Errorf("%s: image reference %q should be fastvm/%s:<version>", tpl.Name, tpl.Image, tpl.Name)
		}
		if tpl.ImageTag() != tpl.Image {
			t.Errorf("%s: ImageTag() = %q, Image = %q", tpl.Name, tpl.ImageTag(), tpl.Image)
		}

		// The Dockerfiles are extracted to hack/templates and built by
		// `make template-images`; keep the two in sync.
		path := filepath.Join("..", "..", "hack", "templates", tpl.Name+".Dockerfile")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read %s: %v", tpl.Name, path, err)
			continue
		}
		if !strings.Contains(string(data), "FROM node:26-bookworm-slim") {
			t.Errorf("%s: Dockerfile does not use the pinned node:26-bookworm-slim base", tpl.Name)
		}
		if !strings.Contains(string(data), "npm install -g") {
			t.Errorf("%s: Dockerfile does not install an npm package", tpl.Name)
		}

		if len(tpl.EnvKeys) == 0 {
			t.Errorf("%s: no env keys documented", tpl.Name)
		}
		if tpl.VCPUs <= 0 || tpl.MemoryMB <= 0 {
			t.Errorf("%s: invalid default resources: %+v", tpl.Name, tpl)
		}
	}
	for _, want := range []string{"claude", "codex", "copilot"} {
		if !seen[want] {
			t.Errorf("missing built-in template %q", want)
		}
		if _, ok := Get(want); !ok {
			t.Errorf("Get(%q) failed", want)
		}
	}
	if _, ok := Get("nope"); ok {
		t.Error("Get(nope) unexpectedly succeeded")
	}
}
