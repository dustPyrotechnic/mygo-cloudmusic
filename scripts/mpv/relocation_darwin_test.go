package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 真实 Mach-O 夹具覆盖递归重定位、重签名与重复打包确定性；仅在 macOS 执行。
func TestDarwinRelocatesAndSignsClosure(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "Player.app")
	frameworks := filepath.Join(app, "Contents", "Frameworks")
	leaf := filepath.Join(frameworks, "libleaf.dylib")
	middle := filepath.Join(frameworks, "libmiddle.dylib")
	main := filepath.Join(frameworks, "libmpv.2.dylib")
	compile := func(name, code, dest, id string, extra ...string) {
		t.Helper()
		source := filepath.Join(root, name+".c")
		putFile(t, source, []byte(code))
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		args := []string{"clang", "-dynamiclib", "-Wl,-headerpad_max_install_names", "-install_name", id, source, "-o", dest}
		if err := command("xcrun", append(args, extra...)...); err != nil {
			t.Fatal(err)
		}
	}
	compile("leaf", "int leaf(void) { return 7; }", leaf, "@rpath/libleaf.dylib")
	compile("middle", "extern int leaf(void); int middle(void) { return leaf(); }", middle, "@executable_path/../Frameworks/libmiddle.dylib", leaf, "-Wl,-rpath,@loader_path")
	compile("main", "extern int middle(void); int mpv_create(void) { return middle(); }", main, "@rpath/libmpv.2.dylib", middle)
	var packed [][]byte
	for i := range 2 {
		stage := filepath.Join(root, string(rune('a'+i)))
		if err := os.Mkdir(stage, 0755); err != nil {
			t.Fatal(err)
		}
		if err := stageDarwin(app, stage, runtime.GOARCH, "-", false); err != nil {
			t.Fatal(err)
		}
		info, err := inspectMachO(filepath.Join(stage, "libmpv.dylib"), runtime.GOARCH)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, dep := range info.deps {
			found = found || dep == "@loader_path/libmiddle.dylib"
		}
		if !found || info.id != "@loader_path/libmpv.dylib" {
			t.Fatalf("重定位失败：%+v", info)
		}
		archive := filepath.Join(root, string(rune('a'+i))+".tar.gz")
		if _, err := archiveFiles(stage, archive, true); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		packed = append(packed, data)
	}
	if !bytes.Equal(packed[0], packed[1]) {
		t.Fatal("相同输入的 ad-hoc 重签名归档不确定")
	}
}
