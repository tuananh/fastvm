package main

import (
	"strings"
	"testing"
)

func TestSplitTemplateArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantName string
		wantArgs []string
		wantErr  bool
	}{
		{name: "name then detach", args: []string{"copilot", "-d"}, wantName: "copilot", wantArgs: []string{"-d"}},
		{name: "detach then name", args: []string{"-d", "copilot"}, wantName: "copilot", wantArgs: []string{"-d"}},
		{name: "flags before name", args: []string{"--name", "foo", "copilot", "--vcpus", "4"}, wantName: "copilot", wantArgs: []string{"--name", "foo", "--vcpus", "4"}},
		{name: "env value", args: []string{"copilot", "--env", "A=B", "-d"}, wantName: "copilot", wantArgs: []string{"--env", "A=B", "-d"}},
		{name: "missing flag value", args: []string{"copilot", "--name"}, wantErr: true},
		{name: "extra positional", args: []string{"copilot", "claude"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, args, err := splitTemplateArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got name=%q args=%v", name, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitTemplateArgs: %v", err)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if strings.Join(args, " ") != strings.Join(tc.wantArgs, " ") {
				t.Errorf("args = %v, want %v", args, tc.wantArgs)
			}
		})
	}
}

func TestParseTemplateStart(t *testing.T) {
	name, opts, err := parseTemplateStart([]string{"copilot", "-d", "--name", "my-vm", "--vcpus", "4", "--memory", "4096", "--env", "A=B"})
	if err != nil {
		t.Fatalf("parseTemplateStart: %v", err)
	}
	if name != "copilot" {
		t.Errorf("name = %q", name)
	}
	if !opts.Detached {
		t.Error("Detached = false, want true")
	}
	if opts.Name != "my-vm" || opts.VCPUs != 4 || opts.MemoryMB != 4096 {
		t.Errorf("opts = %+v", opts)
	}
	if len(opts.Envs) != 1 || opts.Envs[0] != "A=B" {
		t.Errorf("Envs = %v", opts.Envs)
	}
}

func TestRandomSuffix(t *testing.T) {
	a, b := randomSuffix(), randomSuffix()
	if len(a) < 6 {
		t.Errorf("suffix too short: %q", a)
	}
	if a == b {
		t.Errorf("suffix collision: %q", a)
	}
}
