package main

import (
	"debug/macho"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type image struct {
	id     string
	deps   []string
	rpaths []string
	fat    bool
}

type library struct {
	source string
	name   string
	image  image
	links  map[string]*library
}

func inspectMachO(path, arch string) (image, error) {
	cpu := macho.CpuArm64
	if arch == "amd64" {
		cpu = macho.CpuAmd64
	} else if arch != "arm64" {
		return image{}, fmt.Errorf("不支持的架构：%s", arch)
	}
	fat, err := macho.OpenFat(path)
	if err == nil {
		defer fat.Close()
		for _, slice := range fat.Arches {
			if slice.Cpu == cpu {
				result, err := imageInfo(slice.File)
				result.fat = true
				return result, err
			}
		}
		return image{}, fmt.Errorf("%s 缺少 %s 架构", path, arch)
	}
	f, err := macho.Open(path)
	if err != nil {
		return image{}, fmt.Errorf("读取 Mach-O %s: %w", path, err)
	}
	defer f.Close()
	if f.Cpu != cpu {
		return image{}, fmt.Errorf("%s 架构为 %s，需要 %s", path, f.Cpu, arch)
	}
	return imageInfo(f)
}

func imageInfo(f *macho.File) (image, error) {
	var result image
	if f.Type != macho.TypeDylib {
		return result, fmt.Errorf("输入不是 Mach-O 动态库")
	}
	for _, load := range f.Loads {
		raw := load.Raw()
		if len(raw) < 12 {
			return result, fmt.Errorf("无效 Mach-O load command")
		}
		cmd := f.ByteOrder.Uint32(raw[:4]) & 0x7fffffff
		switch cmd {
		case 0xc, 0xd, 0x18, 0x1f, 0x20, 0x23, 0x1c:
			offset := f.ByteOrder.Uint32(raw[8:12])
			if uint64(offset) >= uint64(len(raw)) {
				return result, fmt.Errorf("无效 Mach-O 字符串偏移")
			}
			text := string(raw[offset:])
			end := strings.IndexByte(text, 0)
			if end < 0 {
				return result, fmt.Errorf("无效 Mach-O 字符串终止符")
			}
			text = text[:end]
			switch cmd {
			case 0xd:
				result.id = text
			case 0x1c:
				result.rpaths = append(result.rpaths, text)
			default:
				result.deps = append(result.deps, text)
			}
		}
	}
	if result.id == "" {
		return result, fmt.Errorf("动态库缺少 LC_ID_DYLIB")
	}
	return result, nil
}

func canonical(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func systemLibrary(path string) bool {
	path = filepath.Clean(path)
	return strings.HasPrefix(path, "/usr/lib/") || strings.HasPrefix(path, "/System/Library/")
}

func sourceLayout(input string) (string, string, string, error) {
	input, err := canonical(input)
	if err != nil {
		return "", "", "", err
	}
	info, err := os.Stat(input)
	if err != nil {
		return "", "", "", err
	}
	root := input
	main := ""
	if !info.IsDir() {
		main, root = input, filepath.Dir(input)
	}
	executable := ""
	for parent := root; parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if strings.HasSuffix(strings.ToLower(parent), ".app") {
			root = filepath.Join(parent, "Contents", "Frameworks")
			executable = filepath.Join(parent, "Contents", "MacOS")
			break
		}
	}
	if main == "" {
		candidates := map[string]bool{}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			name := entry.Name()
			if !entry.IsDir() && (name == "libmpv.dylib" || strings.HasPrefix(name, "libmpv.") && strings.HasSuffix(name, ".dylib")) {
				real, err := canonical(path)
				if err != nil {
					return err
				}
				candidates[real] = true
			}
			return nil
		})
		if err != nil {
			return "", "", "", err
		}
		if len(candidates) != 1 {
			return "", "", "", fmt.Errorf("%s 必须包含唯一 libmpv 动态库，找到 %d 个；可改用具体库路径", root, len(candidates))
		}
		for path := range candidates {
			main = path
		}
	}
	return main, root, executable, nil
}

func expandPath(path, loader, executable string) string {
	if strings.HasPrefix(path, "@loader_path/") {
		return filepath.Join(loader, strings.TrimPrefix(path, "@loader_path/"))
	}
	if path == "@loader_path" {
		return loader
	}
	if executable != "" && strings.HasPrefix(path, "@executable_path/") {
		return filepath.Join(executable, strings.TrimPrefix(path, "@executable_path/"))
	}
	if path == "@executable_path" && executable != "" {
		return executable
	}
	return path
}

func resolveDependency(dep, loader, executable string, rpaths []string, index map[string][]string) (string, error) {
	var candidates []string
	if strings.HasPrefix(dep, "@rpath/") {
		for _, path := range rpaths {
			candidates = append(candidates, filepath.Join(path, strings.TrimPrefix(dep, "@rpath/")))
		}
	} else {
		candidates = append(candidates, expandPath(dep, loader, executable))
	}
	for _, path := range candidates {
		if !filepath.IsAbs(path) {
			continue
		}
		if real, err := canonical(path); err == nil {
			return real, nil
		}
	}
	// 发行包常保留构建机的绝对路径；只允许从输入树中无歧义地重定位。
	matches := map[string]bool{}
	for _, path := range index[filepath.Base(dep)] {
		real, err := canonical(path)
		if err != nil {
			return "", err
		}
		matches[real] = true
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("无法唯一定位依赖 %s（候选 %d 个）", dep, len(matches))
	}
	for path := range matches {
		return path, nil
	}
	panic("unreachable")
}

func collectLibraries(main, root, executable, arch string) ([]*library, error) {
	index := map[string][]string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			index[entry.Name()] = append(index[entry.Name()], path)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	seen := map[string]*library{}
	names := map[string]string{}
	var visit func(string, string, []string) (*library, error)
	visit = func(path, name string, inherited []string) (*library, error) {
		if old := seen[path]; old != nil {
			return old, nil
		}
		key := strings.ToLower(name)
		if old := names[key]; old != "" && old != path {
			return nil, fmt.Errorf("平铺文件名冲突 %s：%s 与 %s", name, old, path)
		}
		info, err := inspectMachO(path, arch)
		if err != nil {
			return nil, err
		}
		lib := &library{source: path, name: name, image: info, links: map[string]*library{}}
		seen[path], names[key] = lib, path
		rpaths := make([]string, 0, len(info.rpaths)+len(inherited))
		for _, rpath := range info.rpaths {
			rpaths = append(rpaths, expandPath(rpath, filepath.Dir(path), executable))
		}
		rpaths = append(rpaths, inherited...)
		for _, dep := range info.deps {
			if systemLibrary(dep) {
				continue
			}
			target, err := resolveDependency(dep, filepath.Dir(path), executable, rpaths, index)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			child, err := visit(target, filepath.Base(target), rpaths)
			if err != nil {
				return nil, err
			}
			lib.links[dep] = child
		}
		return lib, nil
	}
	if _, err := visit(main, "libmpv.dylib", nil); err != nil {
		return nil, err
	}
	libs := make([]*library, 0, len(seen))
	for _, lib := range seen {
		libs = append(libs, lib)
	}
	sort.Slice(libs, func(i, j int) bool { return libs[i].name < libs[j].name })
	return libs, nil
}

func command(name string, args ...string) error {
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %q: %w\n%s", name, args, err, output)
	}
	return nil
}

func stageDarwin(input, stage, arch, identity string, timestamp bool) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("macOS 资源须在 macOS 上使用 Xcode 命令行工具打包")
	}
	main, root, executable, err := sourceLayout(input)
	if err != nil {
		return err
	}
	libs, err := collectLibraries(main, root, executable, arch)
	if err != nil {
		return err
	}
	for _, lib := range libs {
		dest := filepath.Join(stage, lib.name)
		if err := copyRegular(lib.source, dest, 0755); err != nil {
			return err
		}
		if lib.image.fat {
			cpu := arch
			if cpu == "amd64" {
				cpu = "x86_64"
			}
			if err := command("lipo", dest, "-thin", cpu, "-output", dest); err != nil {
				return err
			}
		}
		var changes []string
		if lib.image.id != "@loader_path/"+lib.name {
			changes = append(changes, "-id", "@loader_path/"+lib.name)
		}
		for _, dep := range lib.image.deps {
			if child := lib.links[dep]; child != nil && dep != "@loader_path/"+child.name {
				changes = append(changes, "-change", dep, "@loader_path/"+child.name)
			}
		}
		if len(changes) != 0 {
			if err := command("install_name_tool", append(changes, dest)...); err != nil {
				return err
			}
		}
	}
	for _, lib := range libs {
		dest := filepath.Join(stage, lib.name)
		args := []string{"--force", "--sign", identity, "--identifier", lib.name, "--timestamp=none"}
		if timestamp {
			args[len(args)-1] = "--timestamp"
		}
		if err := command("codesign", append(args, dest)...); err != nil {
			return err
		}
		if err := command("codesign", "--verify", "--strict", dest); err != nil {
			return err
		}
		info, err := inspectMachO(dest, arch)
		if err != nil {
			return err
		}
		if info.id != "@loader_path/"+lib.name {
			return fmt.Errorf("install ID 未正确重写：%s", dest)
		}
		for _, dep := range info.deps {
			if systemLibrary(dep) {
				continue
			}
			if !strings.HasPrefix(dep, "@loader_path/") || filepath.Base(dep) != strings.TrimPrefix(dep, "@loader_path/") {
				return fmt.Errorf("资源仍有不可重定位依赖：%s -> %s", dest, dep)
			}
			if _, err := os.Stat(filepath.Join(stage, filepath.Base(dep))); err != nil {
				return fmt.Errorf("资源缺少依赖 %s: %w", dep, err)
			}
		}
	}
	return nil
}
