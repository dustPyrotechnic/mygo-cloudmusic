//go:build !darwin

package mpv

func probeLibrary(path string) error {
	return probeLibraryLocal(path)
}
