package mpv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 下载文件即使哈希正确，也不能将不可加载的解码库安装为可用组件。
func TestDownloadRejectsUnloadableLibrary(t *testing.T) {
	payload := bytes.Repeat([]byte("not a shared library\n"), 128)
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(compressed.Bytes())
	}))
	defer server.Close()
	platform := Platform()
	originalAsset := NativeAssets[platform]
	originalURL, originalCache := baseDownloadURL, cacheDirOverride
	t.Cleanup(func() {
		NativeAssets[platform] = originalAsset
		baseDownloadURL, cacheDirOverride = originalURL, originalCache
	})
	NativeAssets[platform] = nativeFile{
		Name: originalAsset.Name, Asset: "invalid.gz",
		Size: int64(len(payload)), GzSize: int64(compressed.Len()),
		SHA256:   fmt.Sprintf("%x", sha256.Sum256(payload)),
		GzSHA256: fmt.Sprintf("%x", sha256.Sum256(compressed.Bytes())),
	}
	SetBaseURL(server.URL + "/")
	SetCacheDir(t.TempDir())
	if err := Download(t.Context(), nil); err == nil {
		t.Fatal("download accepted an unloadable decoder as an installed component")
	}
	if IsDownloaded() {
		t.Fatal("failed decoder installation was published to the cache")
	}
}

type archiveMember struct {
	name string
	kind byte
	data []byte
}

func testArchive(t *testing.T, members []archiveMember) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, member := range members {
		size := int64(len(member.data))
		if member.kind != tar.TypeReg {
			size = 0
		}
		header := &tar.Header{Name: member.name, Typeflag: member.kind, Size: size, Mode: 0755, Format: tar.FormatUSTAR}
		if member.kind == tar.TypeSymlink || member.kind == tar.TypeLink {
			header.Linkname = "../outside"
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if size != 0 {
			if _, err := writer.Write(member.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func testGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func serveTestAsset(t *testing.T, asset nativeFile, data []byte) string {
	t.Helper()
	originalAsset := NativeAssets[Platform()]
	originalURL, originalCache := baseDownloadURL, cacheDirOverride
	t.Cleanup(func() {
		NativeAssets[Platform()] = originalAsset
		SetBaseURL(originalURL)
		SetCacheDir(originalCache)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不提供 Content-Length，验证流式下载也有压缩大小上限。
		w.(http.Flusher).Flush()
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	asset.URL = server.URL + "/bundle"
	NativeAssets[Platform()] = asset
	SetBaseURL(defaultDownloadURL)
	dir := t.TempDir()
	SetCacheDir(dir)
	return dir
}

// 破损、越界或不完整的资源包不能发布，也不能破坏原缓存。
func TestDownloadRejectsInvalidBundles(t *testing.T) {
	payload := bytes.Repeat([]byte("not a shared library\n"), 128)
	main := archiveMember{name: "libmpv.dylib", kind: tar.TypeReg, data: payload}
	cases := []struct {
		name    string
		members []archiveMember
		mutate  func([]byte, *nativeFile) []byte
		want    string
	}{
		{name: "unloadable", members: []archiveMember{main}, want: "cannot be loaded"},
		{name: "missing-main", members: []archiveMember{{name: "dependency.dylib", kind: tar.TypeReg, data: payload}}, want: "incomplete bundle"},
		{name: "traversal", members: []archiveMember{{name: "../outside", kind: tar.TypeReg, data: payload}}, want: "unsafe archive"},
		{name: "absolute", members: []archiveMember{{name: "/outside", kind: tar.TypeReg, data: payload}}, want: "unsafe archive"},
		{name: "backslash", members: []archiveMember{{name: `..\outside`, kind: tar.TypeReg, data: payload}}, want: "unsafe archive"},
		{name: "symlink", members: []archiveMember{main, {name: "linked", kind: tar.TypeSymlink}}, want: "unsafe archive"},
		{name: "hardlink", members: []archiveMember{main, {name: "linked", kind: tar.TypeLink}}, want: "unsafe archive"},
		{name: "directory", members: []archiveMember{main, {name: "folder", kind: tar.TypeDir}}, want: "unsafe archive"},
		{name: "duplicate", members: []archiveMember{main, main}, want: "unsafe archive"},
		{name: "expanded-too-large", members: []archiveMember{main}, mutate: func(data []byte, asset *nativeFile) []byte {
			asset.Size--
			return data
		}, want: "size"},
		{name: "expanded-too-small", members: []archiveMember{main}, mutate: func(data []byte, asset *nativeFile) []byte {
			asset.Size++
			return data
		}, want: "incomplete bundle"},
		{name: "truncated-tar", members: []archiveMember{main}, mutate: func(data []byte, asset *nativeFile) []byte {
			return data[:len(data)-1024]
		}, want: "incomplete tar"},
		{name: "trailing-tar-data", members: []archiveMember{main}, mutate: func(data []byte, asset *nativeFile) []byte {
			return append(data, 'x')
		}, want: "trailing archive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asset := nativeFile{Name: main.name, Asset: "bundle.tar.gz", Bundle: true, SHA256: fmt.Sprintf("%x", sha256.Sum256(payload))}
			for _, member := range tc.members {
				if member.kind == tar.TypeReg {
					asset.Size += int64(len(member.data))
				}
			}
			archive := testArchive(t, tc.members)
			if tc.mutate != nil {
				archive = tc.mutate(archive, &asset)
			}
			compressed := testGzip(t, archive)
			asset.GzSize = int64(len(compressed))
			asset.GzSHA256 = fmt.Sprintf("%x", sha256.Sum256(compressed))
			dir := serveTestAsset(t, asset, compressed)
			target, err := TargetFile()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				t.Fatal(err)
			}
			previous := []byte("original cached component")
			if err := os.WriteFile(target, previous, 0644); err != nil {
				t.Fatal(err)
			}
			err = Download(t.Context(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Download error = %v, want %q", err, tc.want)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, previous) {
				t.Fatalf("previous cache changed: %q, %v", got, err)
			}
			if IsDownloaded() {
				t.Fatal("invalid component reported ready")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("staging files leaked: %v, %v", entries, err)
			}
		})
	}
}

// 哈希、压缩长度、gzip 尾部及取消错误必须在安装前向调用方返回。
func TestDownloadRejectsDamagedTransfers(t *testing.T) {
	payload := bytes.Repeat([]byte("invalid library"), 100)
	for _, name := range []string{"checksum", "short", "long", "gzip-crc", "gzip-truncated", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			compressed := testGzip(t, payload)
			asset := nativeFile{
				Name: NativeAssets[Platform()].Name, Asset: "library.gz",
				Size: int64(len(payload)), GzSize: int64(len(compressed)),
				SHA256:   fmt.Sprintf("%x", sha256.Sum256(payload)),
				GzSHA256: fmt.Sprintf("%x", sha256.Sum256(compressed)),
			}
			want := "size"
			switch name {
			case "checksum":
				asset.GzSHA256 = strings.Repeat("0", 64)
				want = "checksum"
			case "short":
				asset.GzSize++
			case "long":
				asset.GzSize--
			case "gzip-crc":
				compressed[len(compressed)-8] ^= 0xff
				asset.GzSHA256 = fmt.Sprintf("%x", sha256.Sum256(compressed))
				want = "checksum"
			case "gzip-truncated":
				compressed = compressed[:len(compressed)-4]
				asset.GzSize = int64(len(compressed))
				asset.GzSHA256 = fmt.Sprintf("%x", sha256.Sum256(compressed))
				want = "EOF"
			}
			dir := serveTestAsset(t, asset, compressed)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "cancelled" {
				cancel()
			}
			err := Download(ctx, nil)
			if name == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Download error = %v, want cancellation", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Download error = %v, want %q", err, want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed download left files: %v, %v", entries, err)
			}
		})
	}
}

// 显式镜像覆盖包 URL，旧平台缺少 URL 时仍从原有来源下载。
func TestDownloadURLSelection(t *testing.T) {
	for _, explicitMirror := range []bool{false, true} {
		t.Run(fmt.Sprintf("mirror-%t", explicitMirror), func(t *testing.T) {
			payload := bytes.Repeat([]byte("invalid library"), 100)
			compressed := testGzip(t, payload)
			asset := nativeFile{
				Name: NativeAssets[Platform()].Name, Asset: "library.gz",
				Size: int64(len(payload)), GzSize: int64(len(compressed)),
				SHA256:   fmt.Sprintf("%x", sha256.Sum256(payload)),
				GzSHA256: fmt.Sprintf("%x", sha256.Sum256(compressed)),
			}
			serveTestAsset(t, asset, compressed)
			current := NativeAssets[Platform()]
			localURL := current.URL
			if explicitMirror {
				current.URL = "http://127.0.0.1:1/must-not-be-used"
			} else {
				current.URL = ""
			}
			NativeAssets[Platform()] = current
			SetBaseURL(strings.TrimSuffix(localURL, "bundle"))
			err := Download(t.Context(), nil)
			if err == nil || !strings.Contains(err.Error(), "cannot be loaded") {
				t.Fatalf("local source was not used: %v", err)
			}
		})
	}
}

// 系统库可以打开但没有 MPV 符号时仍必须失败，并保留库和符号信息。
func TestLoadPathRejectsNonMPVLibrary(t *testing.T) {
	var path string
	switch runtime.GOOS {
	case "darwin":
		path = "/usr/lib/libSystem.B.dylib"
	case "linux":
		path = "libc.so.6"
	case "windows":
		path = "kernel32.dll"
	default:
		t.Skip("unsupported host")
	}
	wasAvailable := Available()
	err := LoadPath(path)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "mpv_create") {
		t.Fatalf("LoadPath lost native symbol failure: %v", err)
	}
	if Available() != wasAvailable {
		t.Fatal("failed load changed active library state")
	}
}

// 使用真实依赖包验证可用缓存保留、依赖删除后的失效及重新下载修复。
func TestRealBundleCacheRepair(t *testing.T) {
	path := os.Getenv("MPV_TEST_BUNDLE")
	if path == "" {
		t.Skip("set MPV_TEST_BUNDLE to a real native tar.gz")
	}
	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gzr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer gzr.Close()
	asset := nativeFile{
		Name: NativeAssets[Platform()].Name, Asset: "real-bundle.tar.gz", Bundle: true,
		GzSize: int64(len(compressed)), GzSHA256: fmt.Sprintf("%x", sha256.Sum256(compressed)),
	}
	var dependency string
	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		asset.Size += header.Size
		if header.Name == asset.Name {
			hash := sha256.New()
			if _, err := io.Copy(hash, tr); err != nil {
				t.Fatal(err)
			}
			asset.SHA256 = fmt.Sprintf("%x", hash.Sum(nil))
		} else if dependency == "" && strings.HasSuffix(header.Name, ".dylib") {
			dependency = header.Name
		}
	}
	if dependency == "" || asset.SHA256 == "" {
		t.Fatal("fixture must contain the main library and real dependencies")
	}
	serveTestAsset(t, asset, compressed)
	if err := Download(t.Context(), nil); err != nil {
		t.Fatalf("install real bundle: %v", err)
	}
	if !IsDownloaded() || !Ready() {
		t.Fatal("valid bundle was not ready")
	}
	target, err := TargetFile()
	if err != nil {
		t.Fatal(err)
	}
	compressed[0] ^= 0xff
	if err := Download(t.Context(), nil); err == nil {
		t.Fatal("damaged update was accepted")
	}
	compressed[0] ^= 0xff
	if !IsDownloaded() {
		t.Fatal("failed update discarded the usable cache")
	}
	dependencyPath := filepath.Join(filepath.Dir(target), dependency)
	dependencyBytes, err := os.ReadFile(dependencyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dependencyPath); err != nil {
		t.Fatal(err)
	}
	if IsDownloaded() {
		t.Fatal("missing dependency did not invalidate readiness")
	}
	if err := os.WriteFile(dependencyPath, dependencyBytes, 0755); err != nil {
		t.Fatal(err)
	}
	if !IsDownloaded() {
		t.Fatal("repaired dependency did not invalidate the failed probe")
	}
	if err := os.Remove(dependencyPath); err != nil {
		t.Fatal(err)
	}
	if err := Download(t.Context(), nil); err != nil {
		t.Fatalf("redownload did not repair missing dependency: %v", err)
	}
	if !IsDownloaded() {
		t.Fatal("repaired bundle was not ready")
	}
	if err := Delete(); err != nil {
		t.Fatal(err)
	}
	if IsDownloaded() {
		t.Fatal("deleted bundle remained ready")
	}
}

// 缺少真实动态依赖的包即使主库哈希正确，也不能借用已加载镜像通过安装。
func TestRealBundleRejectsMissingDependency(t *testing.T) {
	path := os.Getenv("MPV_TEST_BUNDLE")
	if path == "" {
		t.Skip("set MPV_TEST_BUNDLE to a real native tar.gz")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzr, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzr.Close()
	var compressed bytes.Buffer
	gzw := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gzw)
	asset := nativeFile{Name: NativeAssets[Platform()].Name, Asset: "missing-dependency.tar.gz", Bundle: true}
	missing := ""
	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != asset.Name && missing == "" && strings.HasSuffix(header.Name, ".dylib") {
			missing = header.Name
			continue
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		asset.Size += header.Size
		var out io.Writer = tw
		hash := sha256.New()
		if header.Name == asset.Name {
			out = io.MultiWriter(tw, hash)
		}
		if _, err := io.Copy(out, tr); err != nil {
			t.Fatal(err)
		}
		if header.Name == asset.Name {
			asset.SHA256 = fmt.Sprintf("%x", hash.Sum(nil))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	if missing == "" {
		t.Fatal("fixture must include a real dynamic dependency")
	}
	asset.GzSize = int64(compressed.Len())
	asset.GzSHA256 = fmt.Sprintf("%x", sha256.Sum256(compressed.Bytes()))
	dir := serveTestAsset(t, asset, compressed.Bytes())
	err = Download(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing dependency was accepted or hidden: %v", err)
	}
	if IsDownloaded() {
		t.Fatal("incomplete bundle reported ready")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("incomplete bundle was published: %v, %v", entries, err)
	}
}
