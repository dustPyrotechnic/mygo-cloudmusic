package mpv

import (
	"sync/atomic"
	"testing"
)

// 关闭实例必须先唤醒并等事件等待退出，再销毁句柄，避免播放退出时崩溃。
func TestInstanceCloseWaitsForEventReader(t *testing.T) {
	original := mpvLib
	t.Cleanup(func() { mpvLib = original })
	entered := make(chan struct{})
	woken := make(chan struct{})
	release := make(chan struct{})
	waitDone := make(chan struct{})
	closeDone := make(chan struct{})
	var active atomic.Bool
	var premature atomic.Bool
	mpvLib = cFuncs{
		waitEvent: func(uintptr, float64) *cEvent {
			active.Store(true)
			close(entered)
			<-release
			active.Store(false)
			return nil
		},
		wakeup: func(uintptr) { close(woken) },
		terminateDestroy: func(uintptr) {
			premature.Store(active.Load())
		},
	}
	inst := &Instance{handle: 1}
	go func() {
		inst.WaitEvent(1)
		close(waitDone)
	}()
	<-entered
	go func() {
		inst.Close()
		close(closeDone)
	}()
	<-woken
	// Close 的旧实现会在事件回调仍阻塞时立即销毁实例。
	// 调度同步由实例锁完成，不依赖 sleep 或时间窗口。
	inst.mu.Lock()
	closed := inst.closed
	inst.mu.Unlock()
	close(release)
	<-waitDone
	<-closeDone
	if !closed || premature.Load() {
		t.Fatalf("destroyed while reading event: closed=%v premature=%v", closed, premature.Load())
	}
	if event, _, _ := inst.WaitEvent(0); event != EventShutdown {
		t.Fatalf("closed instance returned event %d", event)
	}
}
