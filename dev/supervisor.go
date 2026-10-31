package dev

// 进程级热重载：Go 没有"热替换代码"这回事，进程里的机器码换不掉。所以这里
// 的"Go HMR"只能是重新编译 + 换进程，但把换的过程做得足够无感：
//
//   - 先编译，编译通过才动旧进程：写错一个分号时页面还活着，只是代码没更新
//   - 每个版本编译到新的临时文件（Windows 下运行中的 exe 不能覆盖），旧的
//     在进程退出后立刻删掉，长时间开发也不会堆垃圾
//   - 子进程自己 panic 退出不终止监听，改完代码自动拉起
//   - 浏览器侧 WebSocket 断开后轮询 /__wf_ping，服务一回来就整页重载，
//     于是"改 .go → 页面自己变成新代码"

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// debounce 是事件静默窗口：编辑器保存一个文件常触发多个事件，
	// 攒一小会儿再编译，避免连续重启。
	debounce = 120 * time.Millisecond
	// killGrace 是等子进程退出的上限（端口释放需要它真的退出）。
	killGrace = 3 * time.Second
)

// SuperviseOptions 控制 wf dev 的行为。
type SuperviseOptions struct {
	Dir       string    // 要构建/运行的包目录（go build 的 .）
	Tags      string    // go build -tags，空串表示不带 tag
	RunArgs   []string  // 传给子进程的参数
	WatchDir  string    // 监听根目录；默认取 go.mod 所在目录
	BuildArgs []string  // 追加给 go build 的参数（-ldflags 等）
	Out       io.Writer // 日志输出，默认 os.Stdout
}

type supervisor struct {
	opt    SuperviseOptions
	tmpDir string
	log    io.Writer

	mu  sync.Mutex // 保护 run
	run *running
	lm  sync.Mutex // 保护日志不被打断
}

type running struct {
	cmd  *exec.Cmd
	bin  string
	done chan struct{}
}

// Supervise 构建并运行 Dir 指向的包；Go 源码变更时重新编译、重启进程。
// ctx 取消时杀掉子进程并清理临时产物。
func Supervise(ctx context.Context, opt SuperviseOptions) error {
	if opt.Dir == "" {
		opt.Dir = "."
	}
	abs, err := filepath.Abs(opt.Dir)
	if err != nil {
		return err
	}
	opt.Dir = abs
	if opt.Out == nil {
		opt.Out = os.Stdout
	}
	if opt.WatchDir == "" {
		opt.WatchDir = moduleRoot(abs)
	}
	if wd, err := filepath.Abs(opt.WatchDir); err == nil {
		opt.WatchDir = wd
	}

	tmp, err := os.MkdirTemp("", "wf-dev-")
	if err != nil {
		return err
	}

	s := &supervisor{opt: opt, tmpDir: tmp, log: opt.Out}
	defer s.cleanup()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	s.watchDirs(w, opt.WatchDir)

	s.printf("监听 %s（*.go / go.mod）→ 重新编译并重启", filepath.ToSlash(opt.WatchDir))
	s.printf("前端热更由应用内的 dev.DevServer 负责（web/src 变更不会重启进程）")
	if _, err := s.rebuild(); err != nil {
		s.printf("✗ 首次构建失败，仍在监听（修好后自动启动）")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			// 和前端 HMR 不同，这里必须收 Remove/Rename：删掉一个 .go 文件
			// 同样会改变编译结果。重复事件由 settle 合并，不会多编译。
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if fi, statErr := os.Stat(ev.Name); statErr == nil && fi.IsDir() {
				// 新建目录（如 internal/handler）也要纳入监听。
				if ev.Op&fsnotify.Create != 0 {
					s.watchDirs(w, ev.Name)
				}
				continue
			}
			if !isGoChange(ev.Name) {
				continue
			}
			rel, _ := filepath.Rel(opt.WatchDir, ev.Name)
			s.printf("↻ %s 变更", filepath.ToSlash(rel))
			s.settle(w)
			if _, err := s.rebuild(); err != nil {
				s.printf("✗ 编译失败，继续用上一个进程（改好自动重启）")
			}
		case err, ok := <-w.Errors:
			if ok {
				s.printf("⚠ 监听出错: %v", err)
			}
		}
	}
}

// settle 把短时间内的连续事件吃掉，避免一次保存触发多轮重启。
func (s *supervisor) settle(w *fsnotify.Watcher) {
	t := time.NewTimer(debounce)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					s.watchDirs(w, ev.Name)
				}
			}
		}
	}
}

// rebuild 编译一次；成功则换进程，失败则保留旧进程并返回错误。
func (s *supervisor) rebuild() (string, error) {
	start := time.Now()
	bin, out, err := s.build()
	if len(out) > 0 {
		s.printf("%s", strings.TrimRight(string(out), "\n"))
	}
	if err != nil {
		return "", err
	}
	s.printf("✓ 编译通过（%s）", time.Since(start).Round(time.Millisecond))
	if err := s.restart(bin); err != nil {
		return bin, err
	}
	return bin, nil
}

// build 把包编译到临时目录里的新文件。
func (s *supervisor) build() (string, []byte, error) {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	bin := filepath.Join(s.tmpDir, fmt.Sprintf("app-%d%s", time.Now().UnixNano(), suffix))

	args := []string{"build", "-o", bin}
	if s.opt.Tags != "" {
		args = append(args, "-tags", s.opt.Tags)
	}
	args = append(args, s.opt.BuildArgs...)
	args = append(args, ".")

	cmd := exec.Command("go", args...)
	cmd.Dir = s.opt.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", out, err
	}
	return bin, out, nil
}

// restart 停掉旧进程并启动新编译出来的二进制。
func (s *supervisor) restart(bin string) error {
	s.stop()

	cmd := exec.Command(bin, s.opt.RunArgs...)
	cmd.Dir = s.opt.Dir
	cmd.Stdout = s.opt.Out
	cmd.Stderr = s.opt.Out
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), "WF_DEV=1")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}

	r := &running{cmd: cmd, bin: bin, done: make(chan struct{})}
	s.mu.Lock()
	s.run = r
	s.mu.Unlock()
	s.printf("▶ 已启动（pid %d）", cmd.Process.Pid)

	go func() {
		err := cmd.Wait()
		close(r.done)
		s.mu.Lock()
		current := s.run == r
		if current {
			s.run = nil
		}
		s.mu.Unlock()
		// 进程自己崩了（panic / 端口占用）时继续监听，改完代码自动拉起。
		if current {
			if err != nil {
				s.printf("■ 进程已退出（%v），等待文件变更后重启", err)
			} else {
				s.printf("■ 进程已退出，等待文件变更后重启")
			}
		}
	}()
	return nil
}

// stop 杀掉当前子进程并删除它对应的二进制。
func (s *supervisor) stop() {
	s.mu.Lock()
	r := s.run
	s.run = nil
	s.mu.Unlock()
	if r == nil {
		return
	}
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	select {
	case <-r.done:
	case <-time.After(killGrace):
		s.printf("⚠ 进程未在 %s 内退出", killGrace)
	}
	// 退出后再删：Windows 下运行中的 exe 删不掉。
	_ = os.Remove(r.bin)
}

func (s *supervisor) cleanup() {
	s.stop()
	_ = os.RemoveAll(s.tmpDir)
}

// watchDirs 递归监听目录，跳过依赖 / 产物目录。
func (s *supervisor) watchDirs(w *fsnotify.Watcher, root string) {
	_ = filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if p != root && ignoredDir(entry.Name()) {
			return filepath.SkipDir
		}
		if err := w.Add(p); err != nil {
			s.printf("⚠ 无法监听 %s: %v", filepath.ToSlash(p), err)
		}
		return nil
	})
}

// isGoChange 判断这个文件变更是否需要重新编译。
func isGoChange(p string) bool {
	base := filepath.Base(p)
	// 编辑器临时文件 / 隐藏文件（.foo.go.swp、.#main.go、main.go~）
	if strings.HasPrefix(base, ".") || strings.HasSuffix(base, "~") {
		return false
	}
	if base == "go.mod" || base == "go.sum" {
		return true
	}
	// 测试文件不影响二进制
	if strings.HasSuffix(base, "_test.go") {
		return false
	}
	return strings.HasSuffix(base, ".go")
}

// moduleRoot 找 go.mod 所在目录；没有模块时退回 pkgDir。
func moduleRoot(dir string) string {
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return dir
	}
	p := strings.TrimSpace(string(out))
	if p == "" || p == os.DevNull || strings.HasSuffix(p, os.DevNull) {
		return dir
	}
	return filepath.Dir(p)
}

func (s *supervisor) printf(format string, args ...any) {
	s.lm.Lock()
	defer s.lm.Unlock()
	fmt.Fprintf(s.log, "[wf dev] "+format+"\n", args...)
}
