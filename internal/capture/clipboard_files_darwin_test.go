//go:build darwin

package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func stagedClipboardOffer(t *testing.T, files []ClipboardFile, receive func(context.Context, int, *os.File) error) *deferredClipboardOffer {
	t.Helper()
	offer, _, lock, err := newDeferredClipboardOffer(context.Background(), files, receive)
	if err != nil {
		t.Fatal(err)
	}
	directory := offer.root.Name()
	t.Cleanup(func() { offer.dispose(); os.RemoveAll(directory); lock.Close() })
	return offer
}

func TestDeferredClipboardStagingAndCancellation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	files := []ClipboardFile{{Name: "folder", Directory: true}, {Name: "folder/报告.txt", Size: 7},
		{Name: "folder/empty", Size: 0}, {Name: "folder/empty-dir", Directory: true}}
	calls := 0
	offer := stagedClipboardOffer(t, files, func(ctx context.Context, index int, file *os.File) error {
		calls++
		if index == 1 {
			_, err := file.Write([]byte("content"))
			return err
		}
		return nil
	})
	if calls != 0 {
		t.Fatal("content downloaded during publication")
	}
	for _, path := range []string{"folder/报告.txt", "folder/empty"} {
		info, err := offer.root.Stat(path)
		if err != nil || info.Size() != 0 {
			t.Fatal("placeholder is not empty", path, err)
		}
	}
	if err := offer.materialize(); err != nil {
		t.Fatal(err)
	}
	if err := offer.materialize(); err != nil || calls != 2 {
		t.Fatal("repeated paste downloaded the collection again", calls, err)
	}
	got, err := os.ReadFile(filepath.Join(offer.root.Name(), "folder/报告.txt"))
	if err != nil || string(got) != "content" {
		t.Fatal("staged file contents", string(got), err)
	}
	other := stagedClipboardOffer(t, []ClipboardFile{{Name: "never", Size: 3}}, func(context.Context, int, *os.File) error {
		t.Fatal("cancelled clipboard triggered a transfer")
		return nil
	})
	other.cancel()
	if err := other.materialize(); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled offer accepted", err)
	}
}

func TestDeferredClipboardRejectsPartialAndReplacedFiles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	offer := stagedClipboardOffer(t, []ClipboardFile{{Name: "file", Size: 4}}, func(_ context.Context, _ int, file *os.File) error {
		_, err := file.Write([]byte("x"))
		return err
	})
	if err := offer.materialize(); err == nil || offer.ready {
		t.Fatal("partial file accepted")
	}
	replaced := stagedClipboardOffer(t, []ClipboardFile{{Name: "file", Size: 4}}, func(context.Context, int, *os.File) error {
		t.Fatal("replaced placeholder triggered a write")
		return nil
	})
	path := filepath.Join(replaced.root.Name(), "file")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replaced.materialize(); err == nil {
		t.Fatal("replacement file accepted")
	}
	for _, name := range []string{"../escape", "/escape", ".lock", "dir/../escape", "dir\\escape", "\xff"} {
		if _, _, _, err := newDeferredClipboardOffer(context.Background(), []ClipboardFile{{Name: name}}, func(context.Context, int, *os.File) error { return nil }); err == nil {
			t.Fatal("unsafe staging path accepted", name)
		}
	}
}

func TestClipboardTemporaryCleanupRespectsLifetimeLocks(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	base := clipboardTemporaryBase()
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	makeDirectory := func() (string, int) {
		directory, err := os.MkdirTemp(base, "offer-")
		if err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Open(filepath.Join(directory, ".lock"), unix.O_CREAT|unix.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		return directory, fd
	}
	active, fd := makeDirectory()
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	// A native lease duplicates this descriptor; closing Go's original must
	// not release the lock while a Finder reader still owns the duplicate.
	lease, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	fd = -1
	defer func() {
		if lease >= 0 {
			unix.Close(lease)
		}
	}()
	stale, orphan := makeDirectory()
	unix.Close(orphan)
	cleanClipboardTemporaryFiles(base)
	if _, err := os.Stat(active); err != nil {
		t.Fatal("active reader directory was deleted", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("abandoned directory not removed", err)
	}
	unix.Close(lease)
	lease = -1
	cleanClipboardTemporaryFiles(base)
	if _, err := os.Stat(active); !os.IsNotExist(err) {
		t.Fatal("retired reader directory not removed", err)
	}
}
