package runtimeutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplacePrivateFileAtomicallyReplacesPrivateContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	for _, contents := range []string{"before", "after"} {
		if err := ReplacePrivateFile(path, []byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "after" {
		t.Fatalf("contents = %q, want %q", contents, "after")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file permissions = %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions = %v, %v", info, err)
	}
}
