package server

import (
	"context"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// watchFolder reports changes under dir (subfolders too) through Windows'
// directory change notifications until ctx ends or watching fails. started
// is called once the folder is being watched.
func watchFolder(ctx context.Context, dir string, changed, started func()) error {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ev)

	// the system writes into buf and ov after the call returns: both live on
	// the heap, which Go doesn't move
	buf := make([]byte, 64<<10)
	ov := new(windows.Overlapped)
	const mask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE
	first := true
	for {
		*ov = windows.Overlapped{HEvent: ev}
		windows.ResetEvent(ev)
		if err := windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), true, mask, nil, ov, 0); err != nil {
			return err
		}
		if first {
			started()
			first = false
		}
		for {
			r, err := windows.WaitForSingleObject(ev, 1000)
			if err != nil {
				return err
			}
			if r == windows.WAIT_OBJECT_0 {
				break
			}
			if ctx.Err() != nil {
				windows.CancelIoEx(h, ov)
				var n uint32
				windows.GetOverlappedResult(h, ov, &n, true) // let the cancel finish before buf goes
				return nil
			}
		}
		var n uint32
		if err := windows.GetOverlappedResult(h, ov, &n, false); err != nil {
			return err
		}
		if n == 0 || anyRelevant(dir, buf[:n]) { // 0: more changes than buf holds
			changed()
		}
	}
}

// anyRelevant reads a buffer of FILE_NOTIFY_INFORMATION records.
func anyRelevant(dir string, b []byte) bool {
	for off := 0; off+12 <= len(b); {
		info := (*windows.FileNotifyInformation)(unsafe.Pointer(&b[off]))
		name := windows.UTF16ToString(unsafe.Slice(&info.FileName, info.FileNameLength/2))
		added := info.Action == windows.FILE_ACTION_ADDED || info.Action == windows.FILE_ACTION_RENAMED_NEW_NAME
		removed := info.Action == windows.FILE_ACTION_REMOVED || info.Action == windows.FILE_ACTION_RENAMED_OLD_NAME
		if relevantChange(filepath.Join(dir, name), added, removed) {
			return true
		}
		if info.NextEntryOffset == 0 {
			break
		}
		off += int(info.NextEntryOffset)
	}
	return false
}
