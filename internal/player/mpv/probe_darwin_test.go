//go:build darwin

package mpv

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 真实包重复安装不能在宿主中遗留暂存镜像并触发 Objective-C 重复类注册。
func TestRealBundleProbeIsolation(t *testing.T) {
	if os.Getenv("MPV_TEST_BUNDLE") == "" {
		t.Skip("set MPV_TEST_BUNDLE to a real native tar.gz")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), executable, "-test.run=^TestRealBundleCacheRepair$", "-test.count=1", "-test.timeout=3m")
	command.WaitDelay = time.Second
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("repeated real bundle installation failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "is implemented in both") {
		t.Fatalf("probing polluted the host Objective-C registry:\n%s", output)
	}
}
