//go:build darwin

package mpv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	probeArgument    = "--mygo-internal-mpv-probe"
	probeEnvironment = "MYGO_INTERNAL_MPV_PROBE"
)

func init() {
	if len(os.Args) != 3 || os.Args[1] != probeArgument || os.Args[2] == "" || os.Getenv(probeEnvironment) != "1" {
		return
	}
	// 必须同时匹配私有参数和环境标记；不让标记传播到原生库的子进程。
	_ = os.Unsetenv(probeEnvironment)
	if err := probeLibraryLocal(os.Args[2]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func probeLibrary(path string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("mpv: locate probe executable: %w", err)
	}
	// Objective-C/Swift 镜像未必能被 dlclose 卸载，探测必须隔离于播放器进程。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, probeArgument, path)
	command.WaitDelay = time.Second
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, probeEnvironment+"=") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, probeEnvironment+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mpv: isolated probe %s: %w\n%s", path, errors.Join(err, ctx.Err()), strings.TrimSpace(string(output)))
	}
	return nil
}
