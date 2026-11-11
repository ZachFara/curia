package main

import (
	"bytes"
	"testing"
)

func TestVersionFlagPrintsVersion(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"--version"})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute(--version) returned error: %v", err)
	}

	want := "curia version " + version + "\n"
	if got := out.String(); got != want {
		t.Fatalf("--version output = %q, want %q", got, want)
	}
}
