package mpv

import (
	"bytes"
	"debug/macho"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 已加载的旧镜像可能让 dlopen 忽略缺失文件，因此先验证包内依赖闭包。
func validateBundleDependencies(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	dir := filepath.Dir(path)
	pending := []string{filepath.Base(path)}
	seen := make(map[string]bool)
	for len(pending) != 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		libraryPath := filepath.Join(dir, name)
		info, err := os.Lstat(libraryPath)
		if err != nil {
			return fmt.Errorf("mpv: missing dependency %s: %w", libraryPath, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("mpv: dependency is not a regular file: %s", libraryPath)
		}
		library, err := macho.Open(libraryPath)
		if err != nil {
			return fmt.Errorf("mpv: read dependency %s: %w", libraryPath, err)
		}
		dependencies, readErr := bundleImports(library)
		closeErr := library.Close()
		if readErr != nil {
			return fmt.Errorf("mpv: read dependencies of %s: %w", libraryPath, readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		for _, dependency := range dependencies {
			clean := filepath.Clean(dependency)
			if strings.HasPrefix(clean, "/usr/lib/") || strings.HasPrefix(clean, "/System/Library/") {
				continue
			}
			local := strings.TrimPrefix(dependency, "@loader_path/")
			if local == dependency || !flatName(local) {
				return fmt.Errorf("mpv: dependency %s in %s is outside the bundle", dependency, libraryPath)
			}
			pending = append(pending, local)
		}
	}
	return nil
}

func bundleImports(library *macho.File) ([]string, error) {
	var dependencies []string
	for _, load := range library.Loads {
		data := load.Raw()
		if len(data) < 8 {
			return nil, fmt.Errorf("mpv: truncated Mach-O load command")
		}
		switch library.ByteOrder.Uint32(data[:4]) {
		// 普通、弱链接、重导出、延迟及 upward 依赖都必须包含在包内。
		case 0xc, 0x80000018, 0x8000001f, 0x20, 0x80000023:
			if len(data) < 24 {
				return nil, fmt.Errorf("mpv: truncated Mach-O dylib command")
			}
			offset := library.ByteOrder.Uint32(data[8:12])
			if offset < 24 || uint64(offset) >= uint64(len(data)) {
				return nil, fmt.Errorf("mpv: invalid Mach-O dependency offset")
			}
			name := data[offset:]
			end := bytes.IndexByte(name, 0)
			if end <= 0 {
				return nil, fmt.Errorf("mpv: invalid Mach-O dependency name")
			}
			dependencies = append(dependencies, string(name[:end]))
		}
	}
	return dependencies, nil
}
