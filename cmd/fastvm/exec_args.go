package main

import (
	"errors"
	"fmt"
	"strings"
)

// execOptions is the parsed form of `fastvm exec`.
type execOptions struct {
	Ref         string
	Argv        []string
	Interactive bool
}

// parseExecArgs accepts Docker-style interactive flags before or after the
// sandbox name:
//
//	exec -it <sandbox> -- <cmd...>
//	exec -it <sandbox> <cmd...>
//	exec <sandbox> -it -- <cmd...>
//	exec <sandbox> -- <cmd...>          (batch mode, unchanged)
func parseExecArgs(args []string) (execOptions, error) {
	var opts execOptions
	isFlag := func(a string) bool {
		switch a {
		case "-i", "--interactive", "-t", "--tty", "-it", "-ti", "--interactive-tty":
			return true
		}
		return false
	}

	i := 0
	for i < len(args) && isFlag(args[i]) {
		opts.Interactive = true
		i++
	}
	if i >= len(args) {
		return opts, errors.New("exec: sandbox required")
	}
	if strings.HasPrefix(args[i], "-") && args[i] != "-" {
		return opts, execUnknownFlagError(args[i])
	}
	opts.Ref = args[i]
	if opts.Ref == "--" || len(opts.Ref) == 0 {
		return opts, errors.New("exec: sandbox required")
	}
	i++
	rest := args[i:]

	// Optional flags after the sandbox, e.g. `exec myvm -it -- sh`.
	j := 0
	for j < len(rest) && isFlag(rest[j]) {
		opts.Interactive = true
		j++
	}
	rest = rest[j:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}

	if len(rest) == 0 {
		if !opts.Interactive {
			return opts, errors.New("exec: command required")
		}
		rest = []string{"/bin/sh"}
	}
	opts.Argv = rest
	return opts, nil
}

// execUnknownFlagError keeps error text useful for typo'd flags.
func execUnknownFlagError(a string) error {
	return fmt.Errorf("exec: unknown flag %q", a)
}
