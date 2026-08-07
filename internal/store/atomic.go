package store

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// WriteFileAtomic replaces a file's contents such that a reader — or a crash —
// never observes a partial write.
//
// The sequence is: write a sibling temp file, fsync it, rename it over the
// target, then fsync the containing directory. Every step matters:
//
//   - Writing to a temp file means the target keeps its previous contents until
//     the new ones are complete on disk.
//   - fsync on the temp file forces the data out before the rename is durable,
//     otherwise a crash can leave a renamed file full of zeroes. Ordering the
//     two is the whole point; POSIX does not do it for you.
//   - Rename is atomic within a filesystem, so the target is always either the
//     old file or the new one.
//   - fsync on the directory makes the rename itself durable. Without it, a
//     power loss can lose the directory entry and take the file with it.
//
// This is the property "survivable storage in its pod" actually requires. A pod
// evicted mid-write, a node rebooted, a PVC detached — none of them may leave a
// truncated flows.json behind, because a truncated flow file is a plant that
// does not come back up.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file beside %s: %w", path, err)
	}
	tmpName := tmp.Name()

	// From here on, any failure must not leave the temp file behind.
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		// Chmod is unsupported on some filesystems. Not fatal on its own, but
		// it means the file may be more permissive than intended, so say so
		// rather than continuing quietly with a credential file at 0644.
		cleanup()
		return fmt.Errorf("setting mode %o on %s: %w", perm, tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}

	if err := renameReplace(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}

	return syncDir(dir)
}

// renameReplace renames over an existing file.
//
// On Linux — the only platform this ships on — rename(2) is atomic and succeeds
// even while another process has the destination open, so this is a single
// call. Windows is different: MoveFileEx fails with a sharing violation if any
// handle is open on the destination, and Go opens files without
// FILE_SHARE_DELETE. That makes a concurrent reader enough to break a save
// during local development, so retry briefly there rather than making everyone
// develop through a spurious failure. The retry is a no-op on Linux because the
// first attempt does not fail for this reason.
func renameReplace(from, to string) error {
	err := os.Rename(from, to)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	for delay := time.Millisecond; delay <= 64*time.Millisecond; delay *= 2 {
		time.Sleep(delay)
		if err = os.Rename(from, to); err == nil {
			return nil
		}
	}
	return err
}

// syncDir fsyncs a directory so a rename into it is durable.
//
// Windows has no equivalent — opening a directory as a file fails, and NTFS
// makes the metadata update durable as part of the rename. Skipping it there is
// correct, not a compromise; the container this ships in is Linux regardless.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s to sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing directory %s: %w", dir, err)
	}
	return nil
}

// RotateBackups shifts path.bak.N up by one and copies path to path.bak.1,
// keeping at most keep generations.
//
// Backups exist because atomic writes protect against a torn file, not against
// a bad one: a deploy that saves a valid but wrong flow set is just as much a
// loss, and the operator needs somewhere to roll back to.
func RotateBackups(path string, keep int) error {
	if keep <= 0 {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			// Nothing to back up yet.
			return nil
		}
		return err
	}

	// Drop the oldest, then shift each remaining generation up.
	oldest := backupName(path, keep)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", oldest, err)
	}
	for i := keep - 1; i >= 1; i-- {
		from, to := backupName(path, i), backupName(path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotating %s to %s: %w", from, to, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s for backup: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return WriteFileAtomic(backupName(path, 1), data, info.Mode().Perm())
}

func backupName(path string, n int) string {
	return fmt.Sprintf("%s.bak.%d", path, n)
}

// BackupNames returns the backup paths for a file, newest first.
func BackupNames(path string, keep int) []string {
	out := make([]string, 0, keep)
	for i := 1; i <= keep; i++ {
		out = append(out, backupName(path, i))
	}
	return out
}
