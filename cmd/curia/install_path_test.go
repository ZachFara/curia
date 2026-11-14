package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brewTree is <tmp>/Cellar/curia/1/bin/curia (a stand-in file) with
// <tmp>/bin/curia a symlink to it.
func brewTree(t *testing.T) (root, cellar, stable string) {
	t.Helper()
	root = t.TempDir()
	cellar = filepath.Join(root, "Cellar", "curia", "1", "bin", "curia")
	stable = filepath.Join(root, "bin", "curia")
	for _, d := range []string{filepath.Dir(cellar), filepath.Dir(stable)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cellar, []byte("curia 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cellar, stable); err != nil {
		t.Fatal(err)
	}
	return root, cellar, stable
}

func lookup(path string) func(string) (string, error) {
	return func(string) (string, error) {
		if path == "" {
			return "", errors.New("not found")
		}
		return path, nil
	}
}

func TestChooseBinaryPrefersTheSymlinkOnPath(t *testing.T) {
	_, cellar, stable := brewTree(t)
	// The running executable is reported resolved, as on Linux.
	got, err := chooseBinary(cellar, lookup(stable))
	if err != nil || got.path != stable || !got.onPath {
		t.Errorf("chooseBinary = %+v, %v; want %s on PATH", got, err, stable)
	}
}

func TestChooseBinaryWithoutPathUsesInvokedPath(t *testing.T) {
	_, cellar, stable := brewTree(t)
	got, err := chooseBinary(stable, lookup(""))
	if err != nil || got.path != stable || got.onPath {
		t.Errorf("chooseBinary = %+v, %v; want invoked %s, off PATH", got, err, stable)
	}
	got, err = chooseBinary(cellar, lookup(""))
	if err != nil || got.path != cellar || got.onPath {
		t.Errorf("chooseBinary = %+v, %v; want %s, off PATH", got, err, cellar)
	}
}

func TestChooseBinaryIgnoresADifferentFileOnPath(t *testing.T) {
	root, cellar, stable := brewTree(t)
	other := filepath.Join(root, "elsewhere", "curia")
	if err := os.MkdirAll(filepath.Dir(other), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("not us"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := chooseBinary(stable, lookup(other))
	if err != nil || got.path != stable || got.onPath {
		t.Errorf("chooseBinary = %+v, %v; want the invoked %s", got, err, stable)
	}
	got, err = chooseBinary(cellar, lookup(other))
	if err != nil || got.path == other || got.onPath {
		t.Errorf("chooseBinary = %+v, %v; must not record the other file", got, err)
	}
}

func TestChooseBinaryRejectsMissingExecutable(t *testing.T) {
	root := t.TempDir()
	if _, err := chooseBinary(filepath.Join(root, "gone"), lookup("")); err == nil {
		t.Error("chooseBinary of a missing file succeeded")
	}
}

func TestChooseBinaryMakesRelativePathAbsolute(t *testing.T) {
	_, _, stable := brewTree(t)
	t.Chdir(filepath.Dir(stable))
	got, err := chooseBinary(stable, lookup("curia"))
	if err != nil || got.path != stable || !filepath.IsAbs(got.path) {
		t.Errorf("chooseBinary = %+v, %v; want absolute %s", got, err, stable)
	}
}

func TestInstallSayBinary(t *testing.T) {
	for name, tc := range map[string]struct {
		b    binaryPath
		warn bool
	}{
		"stable on PATH":     {binaryPath{"/opt/homebrew/bin/curia", true}, false},
		"Cellar on PATH":     {binaryPath{"/opt/homebrew/Cellar/curia/0.1.0/bin/curia", true}, true},
		"not on PATH":        {binaryPath{"/home/u/curia", false}, true},
		"Cellar not on PATH": {binaryPath{"/opt/homebrew/Cellar/curia/0.1.0/bin/curia", false}, true},
	} {
		var out bytes.Buffer
		installSayBinary(&out, tc.b)
		s := out.String()
		if !strings.Contains(s, "recorded "+tc.b.path) {
			t.Errorf("%s: output lacks the path:\n%s", name, s)
		}
		if got := strings.Contains(s, "will break"); got != tc.warn {
			t.Errorf("%s: warning = %v, want %v:\n%s", name, got, tc.warn, s)
		}
	}
}

func TestInstallStatesTheRecordedPath(t *testing.T) {
	installRecord(t)
	h := installSetup(t, "")
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Binary: recorded "+h.bin+"\n") {
		t.Errorf("install output lacks the recorded path %s:\n%s", h.bin, out)
	}
	if !strings.Contains(out, "put curia on PATH") {
		t.Errorf("the test binary is not on PATH, so install should say how to fix that:\n%s", out)
	}
}
