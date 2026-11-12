//go:build !windows

package server

// installVSR: RTX Video Super Resolution goes through Direct3D 11, which
// only Windows has.
func installVSR(dataDir string) (string, error) { return "", nil }
