package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type metadata struct {
	Name     string
	Asset    string
	Size     int64
	GzSize   int64
	SHA256   string
	GzSHA256 string
	Bundle   bool
	URL      string
}

type paths []string

func (p *paths) String() string { return strings.Join(*p, ", ") }
func (p *paths) Set(value string) error {
	*p = append(*p, value)
	return nil
}

func main() {
	platform := flag.String("platform", "", "目标平台，例如 darwin-arm64、darwin-amd64、linux-amd64")
	input := flag.String("input", "", "输入动态库、库目录或 macOS .app")
	out := flag.String("out", "build/mpv", "资源及 JSON 输出目录")
	sign := flag.String("sign", "-", "macOS codesign 身份，默认 ad-hoc")
	timestamp := flag.Bool("timestamp", false, "正式签名使用安全时间戳；启用后签名不再逐字节可复现")
	baseURL := flag.String("url-base", "", "资源 release 的完整下载 URL 前缀")
	var licenses paths
	flag.Var(&licenses, "license", "许可证/来源文件或目录，可重复；目录汇总为平铺 NOTICES 文本")
	flag.Parse()
	if err := pack(*platform, *input, *out, *sign, *timestamp, *baseURL, licenses); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func pack(platform, input, out, sign string, timestamp bool, baseURL string, licenses []string) error {
	names := map[string]string{
		"darwin-arm64": "libmpv.dylib", "darwin-amd64": "libmpv.dylib",
		"linux-arm64": "libmpv.so.2", "linux-amd64": "libmpv.so.2",
		"windows-arm64": "mpv-2.dll", "windows-amd64": "mpv-2.dll",
	}
	name, ok := names[platform]
	if !ok || input == "" {
		return fmt.Errorf("必须指定有效的 -platform 和 -input")
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(out, ".mpv-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	m := metadata{Name: name, Bundle: strings.HasPrefix(platform, "darwin-")}
	if m.Bundle {
		if sign == "" {
			return fmt.Errorf("macOS 资源必须签名，ad-hoc 请使用 -sign -")
		}
		if err := stageDarwin(input, stage, strings.TrimPrefix(platform, "darwin-"), sign, timestamp); err != nil {
			return err
		}
		for _, license := range licenses {
			if err := stageLicense(license, stage); err != nil {
				return err
			}
		}
	} else {
		if len(licenses) != 0 {
			return fmt.Errorf("单库 gzip 不支持附加许可证；请随 release 单独发布")
		}
		if err := copyRegular(input, filepath.Join(stage, name), 0644); err != nil {
			return err
		}
	}
	m.SHA256, _, err = hashFile(filepath.Join(stage, name))
	if err != nil {
		return err
	}
	m.Asset = platform + "-" + name + ".gz"
	if m.Bundle {
		m.Asset = platform + "-libmpv.tar.gz"
	}
	if baseURL != "" {
		m.URL = strings.TrimRight(baseURL, "/") + "/" + m.Asset
	}
	packed := filepath.Join(stage, "payload.gz")
	m.Size, err = archiveFiles(stage, packed, m.Bundle)
	if err != nil {
		return err
	}
	m.GzSHA256, m.GzSize, err = hashFile(packed)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(map[string]metadata{platform: m}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "metadata.json"), append(data, '\n'), 0644); err != nil {
		return err
	}
	if err := os.Rename(packed, filepath.Join(out, m.Asset)); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(stage, "metadata.json"), filepath.Join(out, platform+".json")); err != nil {
		return err
	}
	fmt.Printf("%s\n%s\n", filepath.Join(out, m.Asset), filepath.Join(out, platform+".json"))
	return nil
}

func copyRegular(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("不是常规文件：%s", src)
	}
	// 大小写不敏感的文件系统也不能吞掉同名库或许可证。
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), filepath.Base(dst)) {
			return fmt.Errorf("输出文件名冲突：%s", dst)
		}
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func stageLicense(source, stage string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyRegular(source, filepath.Join(stage, filepath.Base(source)), 0644)
	}
	name := filepath.Base(filepath.Clean(source)) + "-NOTICES.txt"
	out, err := os.OpenFile(filepath.Join(stage, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := fmt.Fprintln(out, "许可证目录原文汇总；以下标题保留上游相对路径，索引中的文件路径以原目录为准。"); err != nil {
		return err
	}
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("许可证目录包含非常规文件：%s", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "\n===== %s =====\n", filepath.ToSlash(rel)); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	return out.Close()
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), size, err
}

func archiveFiles(dir, destination string, bundle bool) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return 0, err
	}
	gz.Header.OS = 255
	var tw *tar.Writer
	if bundle {
		tw = tar.NewWriter(gz)
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("归档中不允许目录或链接：%s", entry.Name())
		}
		var writer io.Writer = gz
		if tw != nil {
			mode := int64(0644)
			if info.Mode()&0111 != 0 {
				mode = 0755
			}
			header := &tar.Header{
				Name: entry.Name(), Mode: mode, Size: info.Size(),
				Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR,
			}
			if err := tw.WriteHeader(header); err != nil {
				return 0, err
			}
			writer = tw
		}
		in, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return 0, err
		}
		n, copyErr := io.Copy(writer, in)
		closeErr := in.Close()
		if copyErr != nil {
			return 0, copyErr
		}
		if closeErr != nil {
			return 0, closeErr
		}
		total += n
	}
	if tw != nil {
		if err := tw.Close(); err != nil {
			return 0, err
		}
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	return total, f.Close()
}
