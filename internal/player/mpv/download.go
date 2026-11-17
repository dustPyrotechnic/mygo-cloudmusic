package mpv

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultDownloadURL = "https://github.com/vicnoah/mygo-cloudmusic/releases/download/libmpv-" + LibVersion + "/"

var (
	baseDownloadURL  = defaultDownloadURL
	cacheDirOverride string
	dlMu             sync.Mutex
	readinessMu      sync.Mutex
	readiness        = make(map[string]probeResult)
)

// SetBaseURL overrides the download release base URL (useful for testing or mirrors).
func SetBaseURL(u string) {
	dlMu.Lock()
	defer dlMu.Unlock()
	baseDownloadURL = u
}

// SetCacheDir overrides the cache directory for testing.
func SetCacheDir(d string) {
	dlMu.Lock()
	defer dlMu.Unlock()
	readinessMu.Lock()
	defer readinessMu.Unlock()
	cacheDirOverride = d
	clear(readiness)
}

// CacheDir returns the directory path where downloaded libmpv binaries are kept.
func CacheDir() (string, error) {
	if cacheDirOverride != "" {
		return cacheDirOverride, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return filepath.Join(cache, "mygo-cloudmusic", "libmpv-"+LibVersion), nil
}

// TargetFile returns the target library file path inside CacheDir for the current platform.
func TargetFile() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	asset, ok := NativeAssets[Platform()]
	if !ok {
		return "", fmt.Errorf("mpv: unsupported platform %s", Platform())
	}
	if asset.Bundle {
		if !validDigest(asset.GzSHA256) {
			return "", errors.New("mpv: invalid bundle checksum")
		}
		dir = filepath.Join(dir, asset.GzSHA256)
	}
	if !flatName(asset.Name) {
		return "", errors.New("mpv: invalid library name")
	}
	return filepath.Join(dir, asset.Name), nil
}

// IsDownloaded reports whether the library has already been downloaded to the cache directory.
func IsDownloaded() bool {
	readinessMu.Lock()
	defer readinessMu.Unlock()
	p, err := TargetFile()
	return err == nil && cachedProbe(p) == nil
}

// SystemPath 返回可加载且包含全部所需符号的系统库。
func SystemPath() string {
	readinessMu.Lock()
	defer readinessMu.Unlock()
	for _, name := range SystemLibNames() {
		if cachedProbe(name) == nil {
			return name
		}
	}
	return ""
}

func invalidateReadiness() {
	readinessMu.Lock()
	defer readinessMu.Unlock()
	clear(readiness)
}

// Ready 仅将真正可加载的组件视为就绪，探测结果在文件变化时失效。
func Ready() bool {
	return IsDownloaded() || SystemPath() != ""
}

type probeFile struct {
	path string
	info os.FileInfo
}

type probeResult struct {
	files []probeFile
	err   error
}

func sameProbeFile(file probeFile) bool {
	info, err := os.Stat(file.path)
	if file.info == nil {
		return os.IsNotExist(err)
	}
	return err == nil && os.SameFile(file.info, info) &&
		file.info.Size() == info.Size() && file.info.Mode() == info.Mode() &&
		file.info.ModTime() == info.ModTime()
}

func cachedProbe(path string) error {
	if result, ok := readiness[path]; ok {
		unchanged := true
		for _, file := range result.files {
			if !sameProbeFile(file) {
				unchanged = false
				break
			}
		}
		if unchanged {
			return result.err
		}
	}

	// 同时记录相邻依赖；修复或删除依赖后不复用旧的成功/失败结果。
	result := probeResult{}
	record := func(name string) {
		info, _ := os.Stat(name)
		result.files = append(result.files, probeFile{path: name, info: info})
	}
	record(path)
	dir := filepath.Dir(path)
	record(dir)
	target, _ := TargetFile()
	asset := NativeAssets[Platform()]
	if asset.Bundle && path == target {
		entries, err := os.ReadDir(dir)
		if err != nil {
			result.err = fmt.Errorf("mpv: read installed bundle: %w", err)
		}
		var total int64
		for _, entry := range entries {
			name := filepath.Join(dir, entry.Name())
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				result.err = fmt.Errorf("mpv: invalid installed bundle member %s", name)
				break
			}
			total += info.Size()
			if entry.Name() != filepath.Base(path) {
				record(name)
			}
		}
		if total != asset.Size {
			result.err = errors.New("mpv: incomplete installed bundle")
		}
		if result.err == nil {
			result.err = validateBundleDependencies(path)
		}
	}
	if result.err == nil {
		result.err = probeLibrary(path)
	}
	readiness[path] = result
	return result.err
}

// Download 校验完整资源及实际加载能力后，才原子发布到缓存。
func Download(ctx context.Context, onProgress func(done, total int64)) error {
	dlMu.Lock()
	defer dlMu.Unlock()
	defer invalidateReadiness()

	asset, ok := NativeAssets[Platform()]
	if !ok {
		return fmt.Errorf("mpv: no precompiled asset for platform %s", Platform())
	}
	if !flatName(asset.Name) || asset.Size <= 0 || asset.GzSize <= 0 ||
		asset.Size > 1<<40 || asset.GzSize > 1<<40 ||
		!validDigest(asset.SHA256) || !validDigest(asset.GzSHA256) {
		return errors.New("mpv: invalid download metadata")
	}
	targetPath, err := TargetFile()
	if err != nil {
		return err
	}
	dir, err := CacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mpv: mkdir cache: %w", err)
	}
	stage, err := os.MkdirTemp(dir, ".mpv-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	archive, err := os.CreateTemp(dir, ".mpv-download-")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	downloadURL := asset.URL
	if downloadURL == "" || baseDownloadURL != defaultDownloadURL {
		downloadURL = baseDownloadURL + asset.Asset
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mpv: fetch %s: %w", downloadURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mpv: download failed: HTTP %s", resp.Status)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != asset.GzSize {
		return fmt.Errorf("mpv: compressed size mismatch: got %d, want %d", resp.ContentLength, asset.GzSize)
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, hasher), io.LimitReader(&progressReader{
		r: resp.Body, total: asset.GzSize, onProgress: onProgress,
	}, asset.GzSize+1))
	if err != nil {
		return fmt.Errorf("mpv: download: %w", err)
	}
	if n != asset.GzSize {
		return fmt.Errorf("mpv: compressed size mismatch: got %d, want %d", n, asset.GzSize)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != asset.GzSHA256 {
		return errors.New("mpv: compressed checksum mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	compressed := bufio.NewReader(archive)
	gzr, err := gzip.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("mpv: gzip reader: %w", err)
	}
	defer gzr.Close()
	gzr.Multistream(false)
	if asset.Bundle {
		err = unpackBundle(gzr, stage, asset)
	} else {
		err = unpackLibrary(gzr, filepath.Join(stage, asset.Name), asset.Size)
	}
	if err != nil {
		return err
	}
	if _, err := compressed.ReadByte(); err != io.EOF {
		return errors.New("mpv: trailing compressed data")
	}
	candidate := filepath.Join(stage, asset.Name)
	file, err := os.Open(candidate)
	if err != nil {
		return fmt.Errorf("mpv: missing library in archive: %w", err)
	}
	hasher.Reset()
	_, copyErr := io.Copy(hasher, file)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != asset.SHA256 {
		return errors.New("mpv: library checksum mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if asset.Bundle {
		if err := validateBundleDependencies(candidate); err != nil {
			return fmt.Errorf("mpv: downloaded component cannot be loaded: %w", err)
		}
	}
	if err := probeLibrary(candidate); err != nil {
		return fmt.Errorf("mpv: downloaded component cannot be loaded: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	readinessMu.Lock()
	defer readinessMu.Unlock()
	if asset.Bundle {
		return publishBundle(stage, filepath.Dir(targetPath))
	}
	return os.Rename(candidate, targetPath)
}

func flatName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\:\x00") && filepath.Base(name) == name
}

func validDigest(value string) bool {
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size && value == strings.ToLower(value)
}

func unpackLibrary(r io.Reader, path string, size int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(file, io.LimitReader(r, size+1))
	err = errors.Join(copyErr, file.Sync(), file.Close())
	if err != nil {
		return fmt.Errorf("mpv: unpack library: %w", err)
	}
	if n != size {
		return fmt.Errorf("mpv: unpacked size mismatch: got %d, want %d", n, size)
	}
	return nil
}

func unpackBundle(r io.Reader, dir string, asset nativeFile) error {
	const maxFiles = 1024
	// 除文件内容外，仅允许有界数量的 tar 头、填充及结束块。
	counted := &progressReader{r: io.LimitReader(r, asset.Size+maxFiles*1024+1025)}
	reader := tar.NewReader(counted)
	names := make(map[string]bool)
	var total, padding int64
	for {
		before := counted.done
		header, err := reader.Next()
		if err == io.EOF {
			if counted.done-before != padding+1024 {
				return errors.New("mpv: incomplete tar archive")
			}
			break
		}
		if err != nil {
			return fmt.Errorf("mpv: read bundle: %w", err)
		}
		if header.Typeflag != tar.TypeReg || !flatName(header.Name) ||
			names[header.Name] || len(names) >= maxFiles {
			return fmt.Errorf("mpv: unsafe archive member %q", header.Name)
		}
		if header.Size < 0 || header.Size > asset.Size-total {
			return errors.New("mpv: bundle unpacked size exceeds metadata")
		}
		names[header.Name] = true
		if err := unpackLibrary(reader, filepath.Join(dir, header.Name), header.Size); err != nil {
			return err
		}
		total += header.Size
		padding = (512 - header.Size%512) % 512
	}
	if total != asset.Size || !names[asset.Name] {
		return errors.New("mpv: incomplete bundle contents")
	}
	// 读到 gzip EOF 才能验证 CRC；tar 结束块之后不接受隐藏的成员或数据。
	var extra [1]byte
	if n, err := counted.Read(extra[:]); n != 0 || err != io.EOF {
		if err != nil && err != io.EOF {
			return fmt.Errorf("mpv: finish bundle: %w", err)
		}
		return errors.New("mpv: trailing archive data")
	}
	return nil
}

func publishBundle(stage, target string) error {
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		return os.Rename(stage, target)
	} else if err != nil {
		return err
	}
	// 旧目录保留到新包全部验证完成；发布失败时恢复原目录。
	backup := stage + "-previous"
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(stage, target); err != nil {
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return fmt.Errorf("mpv: publish: %w; restore %s: %v", err, backup, restoreErr)
		}
		return err
	}
	return os.RemoveAll(backup)
}

// Delete removes the downloaded libmpv files from the cache directory.
func Delete() error {
	dlMu.Lock()
	defer dlMu.Unlock()
	readinessMu.Lock()
	defer readinessMu.Unlock()
	defer clear(readiness)
	dir, err := CacheDir()
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

type progressReader struct {
	r          io.Reader
	done       int64
	total      int64
	onProgress func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.onProgress != nil {
		p.onProgress(p.done, p.total)
	}
	return n, err
}
