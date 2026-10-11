package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		args     []string
		code     int
		inStdout string
		inStderr string
	}{
		{[]string{"version"}, 0, "tether ", ""},
		{[]string{"--version"}, 0, "tether ", ""},
		{[]string{"help"}, 0, "Usage:", ""},
		{nil, 2, "", "Usage:"},
		{[]string{"bogus"}, 2, "", "unknown command"},
		{[]string{"serve", "--token", "short"}, 2, "", "at least 16"},
		{[]string{"serve", "-h"}, 0, "", "-insecure-allow-public"},
		{[]string{"serve", "--addr", "0.0.0.0:0", "--chrome", "127.0.0.1:1"}, 1, "", "refusing to listen"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(tt.args, &out, &errb)
			if code != tt.code || !strings.Contains(out.String(), tt.inStdout) || !strings.Contains(errb.String(), tt.inStderr) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errb.String())
			}
		})
	}
}

func TestCA(t *testing.T) {
	var out, errb bytes.Buffer
	dir := t.TempDir()
	if code := run([]string{"ca", "--data-dir", dir}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), dir) {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
}
