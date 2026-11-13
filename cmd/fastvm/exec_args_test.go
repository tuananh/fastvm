package main

import (
	"reflect"
	"testing"
)

func TestParseExecArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantRef     string
		wantArgv    []string
		interactive bool
		wantErr     bool
	}{
		{name: "batch with dashdash", args: []string{"vm", "--", "echo", "hi"}, wantRef: "vm", wantArgv: []string{"echo", "hi"}},
		{name: "batch without dashdash", args: []string{"vm", "echo", "hi"}, wantRef: "vm", wantArgv: []string{"echo", "hi"}},
		{name: "flags before ref", args: []string{"-it", "vm", "--", "/bin/sh"}, wantRef: "vm", wantArgv: []string{"/bin/sh"}, interactive: true},
		{name: "flags after ref", args: []string{"vm", "-it", "--", "/bin/sh"}, wantRef: "vm", wantArgv: []string{"/bin/sh"}, interactive: true},
		{name: "separate flags", args: []string{"-i", "-t", "vm", "sh"}, wantRef: "vm", wantArgv: []string{"sh"}, interactive: true},
		{name: "interactive default shell", args: []string{"-it", "vm"}, wantRef: "vm", wantArgv: []string{"/bin/sh"}, interactive: true},
		{name: "command flag-looking arg", args: []string{"vm", "echo", "-it"}, wantRef: "vm", wantArgv: []string{"echo", "-it"}},
		{name: "missing sandbox", args: []string{"-it"}, wantErr: true},
		{name: "missing command", args: []string{"vm"}, wantErr: true},
		{name: "unknown leading flag", args: []string{"--nope", "vm"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseExecArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", opts)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseExecArgs: %v", err)
			}
			if opts.Ref != tc.wantRef || opts.Interactive != tc.interactive || !reflect.DeepEqual(opts.Argv, tc.wantArgv) {
				t.Errorf("opts = %+v, want ref=%q argv=%v interactive=%v", opts, tc.wantRef, tc.wantArgv, tc.interactive)
			}
		})
	}
}
