package dev

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIsGoChange(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"internal/handler/user.go", true},
		{"go.mod", true},
		{"go.sum", true},
		{"main_test.go", false}, // 测试文件不影响二进制
		{"web/src/App.tsx", false},
		{".main.go.swp", false}, // 编辑器临时文件
		{"main.go~", false},
		{"README.md", false},
	}
	for _, c := range cases {
		if got := isGoChange(filepath.FromSlash(c.path)); got != c.want {
			t.Errorf("isGoChange(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// lockedBuf 让测试能在子进程 stdout 与 supervisor 日志并发写入时安全读取。
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestSuperviseRebuildsOnGoChange 端到端验证"改 .go → 重新编译 → 换进程"：
// 起一个真实的小程序，改源码后检查它确实以新代码重新跑起来了。
// 顺带覆盖删除 .go（同样会改变编译结果）与删掉后重新加回来。
func TestSuperviseRebuildsOnGoChange(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过需要 go build 的集成测试")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("没有 go 命令")
	}

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module wfdevtest\n\ngo 1.21\n")
	writeTestFile(t, filepath.Join(dir, "main.go"), appSrc())
	writeTestFile(t, filepath.Join(dir, "version.go"), versionSrc("v1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := &lockedBuf{}
	done := make(chan error, 1)
	go func() {
		done <- Supervise(ctx, SuperviseOptions{
			Dir:      dir,
			WatchDir: dir,
			Out:      out,
		})
	}()

	waitFor(t, 60*time.Second, "首次启动写出版本 v1", func() bool {
		return readFile(t, filepath.Join(dir, "out.txt")) == "v1"
	})

	// 改源码：应触发重新编译 + 重启，新进程写出 v2
	writeTestFile(t, filepath.Join(dir, "version.go"), versionSrc("v2"))
	waitFor(t, 60*time.Second, "重新编译后写出版本 v2", func() bool {
		return readFile(t, filepath.Join(dir, "out.txt")) == "v2"
	})

	// 删源码：编译结果变了，必须重新编译（而不是只在写入时才响应）
	if err := os.Remove(filepath.Join(dir, "version.go")); err != nil {
		t.Fatalf("删除 version.go: %v", err)
	}
	waitFor(t, 60*time.Second, "删除 .go 后触发编译失败", func() bool {
		return bytes.Contains([]byte(out.String()), []byte("编译失败"))
	})
	// 编译失败时旧进程仍在服务
	if got := readFile(t, filepath.Join(dir, "out.txt")); got != "v2" {
		t.Fatalf("删除文件导致编译失败时，旧进程不应受影响，out.txt=%q", got)
	}

	// 加回来：应自动恢复
	writeTestFile(t, filepath.Join(dir, "version.go"), versionSrc("v3"))
	waitFor(t, 60*time.Second, "重新添加后写出版本 v3", func() bool {
		return readFile(t, filepath.Join(dir, "out.txt")) == "v3"
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise 返回错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel 后 Supervise 没有退出")
	}

	if logText := out.String(); !bytes.Contains([]byte(logText), []byte("编译通过")) {
		t.Fatalf("日志里应能看到编译结果:\n%s", logText)
	}
}

// TestSuperviseKeepsOldProcessOnBuildError 编译失败时不能把正在跑的进程杀掉。
func TestSuperviseKeepsOldProcessOnBuildError(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过需要 go build 的集成测试")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("没有 go 命令")
	}

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module wfdevtest\n\ngo 1.21\n")
	writeTestFile(t, filepath.Join(dir, "main.go"), appSrc())
	writeTestFile(t, filepath.Join(dir, "version.go"), versionSrc("v1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := &lockedBuf{}
	go func() {
		_ = Supervise(ctx, SuperviseOptions{Dir: dir, WatchDir: dir, Out: out})
	}()

	waitFor(t, 60*time.Second, "首次启动写出版本 v1", func() bool {
		return readFile(t, filepath.Join(dir, "out.txt")) == "v1"
	})

	// 写一段编译不过的代码
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() { this is not go }\n")
	waitFor(t, 60*time.Second, "日志出现编译失败", func() bool {
		return bytes.Contains([]byte(out.String()), []byte("编译失败"))
	})

	// 旧进程还活着：out.txt 仍是 v1，且进程没有退出日志
	if got := readFile(t, filepath.Join(dir, "out.txt")); got != "v1" {
		t.Fatalf("编译失败不应影响旧进程，out.txt=%q", got)
	}
	if bytes.Contains([]byte(out.String()), []byte("进程已退出")) {
		t.Fatalf("编译失败时旧进程被杀了:\n%s", out.String())
	}
}

func appSrc() string {
	return `package main

import (
	"os"
	"time"
)

func main() {
	_ = os.WriteFile("out.txt", []byte(version()), 0o644)
	time.Sleep(time.Hour)
}
`
}

func versionSrc(v string) string {
	return "package main\n\nfunc version() string { return \"" + v + "\" }\n"
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}
