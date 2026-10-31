package voice

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingLogKeepsTwoBackupsAndPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.log")
	w, err := openRotatingLog(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for _, line := range []string{"first\n", "second\n", "third\n", "fourth\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"debug.log":   "fourth\n",
		"debug.log.1": "third\n",
		"debug.log.2": "second\n",
	} {
		got, err := os.ReadFile(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		info, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("%s permissions = %o, want private", name, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("unexpected third backup: %v", err)
	}
}
