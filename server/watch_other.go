//go:build !windows

package server

import (
	"context"
	"fmt"
	"hash/fnv"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

var watchPoll = 3 * time.Minute

// watchFolder looks over the video files under dir every few minutes and
// reports when they changed, until ctx ends or dir can't be read. started
// is called once the first look worked.
func watchFolder(ctx context.Context, dir string, changed, started func()) error {
	sig, err := folderSignature(dir)
	if err != nil {
		return err
	}
	started()
	t := time.NewTicker(watchPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		s, err := folderSignature(dir)
		if err != nil {
			return err
		}
		if s != sig {
			sig = s
			changed()
		}
	}
}

// folderSignature sums up the video files under dir: paths, sizes, times.
func folderSignature(dir string) (uint64, error) {
	h := fnv.New64a()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil
		}
		if d.IsDir() || !videoExt[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		if info, err := d.Info(); err == nil {
			fmt.Fprintf(h, "%s|%d|%d\n", path, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return h.Sum64(), err
}
