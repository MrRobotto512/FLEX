package server

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
)

// flexvsr.exe, built from tools/vsr/flexvsr.cpp (see there to rebuild).
//
//go:embed vsr/flexvsr.exe
var vsrExe []byte

// installVSR writes flexvsr.exe into the data folder (bin\) when it isn't
// there or differs from this build's, and returns its path.
func installVSR(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "bin")
	file := filepath.Join(dir, "flexvsr.exe")
	if b, err := os.ReadFile(file); err == nil && bytes.Equal(b, vsrExe) {
		return file, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := file + ".new"
	if err := os.WriteFile(tmp, vsrExe, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return file, nil
}
