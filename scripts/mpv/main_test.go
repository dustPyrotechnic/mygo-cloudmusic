package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func putFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// 只构造 load commands，依赖收集测试不需要编译或执行动态库。
func putMachO(t *testing.T, path string, cpu macho.Cpu, deps, rpaths []string) {
	t.Helper()
	var commands bytes.Buffer
	add := func(cmd uint32, text string, base int) {
		size := (base + len(text) + 1 + 7) &^ 7
		data := make([]byte, size)
		binary.LittleEndian.PutUint32(data, cmd)
		binary.LittleEndian.PutUint32(data[4:], uint32(size))
		binary.LittleEndian.PutUint32(data[8:], uint32(base))
		copy(data[base:], text)
		commands.Write(data)
	}
	add(0xd, "@rpath/"+filepath.Base(path), 24)
	for _, dep := range deps {
		add(0xc, dep, 24)
	}
	for _, rpath := range rpaths {
		add(0x8000001c, rpath, 12)
	}
	header := []uint32{0xfeedfacf, uint32(cpu), 0, uint32(macho.TypeDylib), uint32(1 + len(deps) + len(rpaths)), uint32(commands.Len()), 0, 0}
	var data bytes.Buffer
	if err := binary.Write(&data, binary.LittleEndian, header); err != nil {
		t.Fatal(err)
	}
	data.Write(commands.Bytes())
	putFile(t, path, data.Bytes())
}

// 旧工具只保存主库；这里覆盖递归依赖、循环、系统库与 app 内路径解析。
func TestCollectAppDependencyClosure(t *testing.T) {
	app := filepath.Join(t.TempDir(), "播放器.app")
	root := filepath.Join(app, "Contents", "Frameworks")
	main := filepath.Join(root, "libmpv.2.dylib")
	putMachO(t, main, macho.CpuArm64, []string{"@executable_path/../Frameworks/liba.dylib", "/usr/lib/libSystem.B.dylib"}, []string{"@loader_path"})
	putMachO(t, filepath.Join(root, "liba.dylib"), macho.CpuArm64, []string{"@rpath/libb.dylib"}, nil)
	putMachO(t, filepath.Join(root, "libb.dylib"), macho.CpuArm64, []string{"@loader_path/liba.dylib"}, nil)
	if err := os.Symlink("libmpv.2.dylib", filepath.Join(root, "libmpv.dylib")); err != nil {
		t.Fatal(err)
	}
	entry, search, executable, err := sourceLayout(app)
	if err != nil {
		t.Fatal(err)
	}
	libs, err := collectLibraries(entry, search, executable, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 3 || libs[0].name != "liba.dylib" || libs[2].name != "libmpv.dylib" || len(libs[1].links) != 1 {
		t.Fatalf("依赖闭包不完整：%+v", libs)
	}
}

// 缺库、同名不同库和错误架构不能被静默忽略后产出伪完整包。
func TestCollectRejectsInvalidClosure(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"missing", "无法唯一定位"}, {"ambiguous", "候选 2"},
		{"collision", "文件名冲突"}, {"architecture", "架构"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			main := filepath.Join(root, "libmpv.dylib")
			deps := []string{"@rpath/liba.dylib"}
			switch test.name {
			case "ambiguous":
				putMachO(t, filepath.Join(root, "one", "liba.dylib"), macho.CpuArm64, nil, nil)
				putMachO(t, filepath.Join(root, "two", "liba.dylib"), macho.CpuArm64, nil, nil)
			case "collision":
				deps = []string{"@loader_path/one/liba.dylib", "@loader_path/two/liba.dylib"}
				putMachO(t, filepath.Join(root, "one", "liba.dylib"), macho.CpuArm64, nil, nil)
				putMachO(t, filepath.Join(root, "two", "liba.dylib"), macho.CpuArm64, nil, nil)
			case "architecture":
				putMachO(t, filepath.Join(root, "liba.dylib"), macho.CpuAmd64, nil, nil)
			}
			putMachO(t, main, macho.CpuArm64, deps, nil)
			_, err := collectLibraries(main, root, "", "arm64")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("期望 %q，得到 %v", test.want, err)
			}
		})
	}
}

// 两种架构都可以读取静态依赖闭包，不把系统库或 install ID 当作外部依赖。
func TestCollectSelfContainedLibraries(t *testing.T) {
	for arch, cpu := range map[string]macho.Cpu{"arm64": macho.CpuArm64, "amd64": macho.CpuAmd64} {
		root := t.TempDir()
		main := filepath.Join(root, "libmpv.dylib")
		putMachO(t, main, cpu, []string{"/System/Library/Frameworks/CoreAudio.framework/Versions/A/CoreAudio"}, nil)
		libs, err := collectLibraries(main, root, "", arch)
		if err != nil || len(libs) != 1 {
			t.Fatalf("%s: %v, %v", arch, libs, err)
		}
	}
}

// 输出必须与源文件时间和创建顺序无关，并满足运行时的平铺 USTAR 协议。
func TestArchiveDeterministicFlatFiles(t *testing.T) {
	root := t.TempDir()
	var outputs [][]byte
	for i := range 2 {
		dir := filepath.Join(root, string(rune('a'+i)))
		putFile(t, filepath.Join(dir, "libmpv.dylib"), []byte("main library"))
		putFile(t, filepath.Join(dir, "LICENSE.txt"), []byte("license"))
		if err := os.Chtimes(filepath.Join(dir, "libmpv.dylib"), time.Unix(int64(i+100), 0), time.Unix(int64(i+200), 0)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, string(rune('a'+i))+".tar.gz")
		size, err := archiveFiles(dir, path, true)
		if err != nil || size != 19 {
			t.Fatalf("大小 %d，错误 %v", size, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, data)
	}
	if !bytes.Equal(outputs[0], outputs[1]) {
		t.Fatal("归档不能逐字节复现")
	}
	gz, err := gzip.NewReader(bytes.NewReader(outputs[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[len(data)-1024:], make([]byte, 1024)) {
		t.Fatal("tar 缺少双零尾块")
	}
	reader := tar.NewReader(bytes.NewReader(data))
	for _, name := range []string{"LICENSE.txt", "libmpv.dylib"} {
		header, err := reader.Next()
		if err != nil || header.Name != name || header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.ModTime.Unix() != 0 || header.Uid != 0 || header.Gid != 0 {
			t.Fatalf("无效成员：%+v, %v", header, err)
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("出现多余成员：%v", err)
	}
}

// 原 Windows/Linux 单文件 gzip 能力继续可用，元数据不再重写 natives.go。
func TestPackSingleLibraryMetadata(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "input.so")
	putFile(t, source, []byte("library payload"))
	out := filepath.Join(root, "output")
	if err := pack("linux-amd64", source, out, "-", false, "", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "linux-amd64.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]metadata
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	m := records["linux-amd64"]
	hash, size, err := hashFile(source)
	if err != nil {
		t.Fatal(err)
	}
	packedHash, packedSize, err := hashFile(filepath.Join(out, m.Asset))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "libmpv.so.2" || m.Bundle || m.URL != "" || m.SHA256 != hash || m.Size != size || m.GzSHA256 != packedHash || m.GzSize != packedSize {
		t.Fatalf("元数据不匹配：%+v", m)
	}
}

// 平铺归档不接受目录或链接，附加许可证也不能覆盖动态库。
func TestRejectUnsafeArchiveMembers(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	putFile(t, source, []byte("source"))
	dir := filepath.Join(root, "stage")
	putFile(t, filepath.Join(dir, "libmpv.dylib"), []byte("library"))
	if err := copyRegular(source, filepath.Join(dir, "LIBMPV.DYLIB"), 0644); err == nil {
		t.Fatal("允许了大小写文件名冲突")
	}
	if err := os.Symlink(source, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveFiles(dir, filepath.Join(root, "archive.gz"), true); err == nil {
		t.Fatal("允许了链接成员")
	}
}

// 嵌套许可证必须完整保留组件归属和索引，不能为满足平铺协议而丢文件。
func TestLicenseDirectoryPreservesEveryNotice(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "licenses")
	putFile(t, filepath.Join(source, "mpv", "LICENSE"), []byte("mpv license"))
	putFile(t, filepath.Join(source, "ffmpeg", "LICENSE"), []byte("ffmpeg license"))
	putFile(t, filepath.Join(source, "index.json"), []byte(`{"libraries":["mpv","ffmpeg"]}`))
	stage := filepath.Join(root, "stage")
	if err := os.Mkdir(stage, 0755); err != nil {
		t.Fatal(err)
	}
	if err := stageLicense(source, stage); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stage, "licenses-NOTICES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"mpv/LICENSE", "mpv license", "ffmpeg/LICENSE", "ffmpeg license", `{"libraries":["mpv","ffmpeg"]}`} {
		if !bytes.Contains(data, []byte(text)) {
			t.Fatalf("遗漏许可证内容 %q", text)
		}
	}
	if err := stageLicense(source, stage); err == nil {
		t.Fatal("重复许可证文件名未失败")
	}
}
