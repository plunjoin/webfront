package compiler

// install.go：纯 Go 的 lockfile 驱动安装器（§6 #2 落地形态）。
// 不做版本解析（交给 npm/pnpm 一次性生成 package-lock.json），只接管：
//   读 lockfile 的 resolved(URL) + integrity(sha512) → 下载 tarball → 校验 → 解压。
// 全程零 Node、零 CGO。
//
// 唯一布局（版本化 store）：
//   <modulesDir>/.store/<name>@<ver>/...   多版本共存，配合 StoreResolver
//   <modulesDir>/.store/manifest.json      root 依赖 + 每个包的依赖版本表
// 构建时自动挂载 StoreResolver 解析，无需任何额外配置。

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type lockPackage struct {
	Version              string            `json:"version"`
	Resolved             string            `json:"resolved"`
	Integrity            string            `json:"integrity"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	Dev                  bool              `json:"dev"`
	Link                 bool              `json:"link"`
}

type lockfile struct {
	LockfileVersion int                    `json:"lockfileVersion"`
	Packages        map[string]lockPackage `json:"packages"`
}

func readLock(root string) (lockfile, error) {
	data, err := os.ReadFile(filepath.Join(root, "package-lock.json"))
	if err != nil {
		return lockfile{}, fmt.Errorf("read package-lock.json: %w", err)
	}
	var lf lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		return lockfile{}, fmt.Errorf("parse lockfile: %w", err)
	}
	if len(lf.Packages) == 0 {
		return lockfile{}, fmt.Errorf("lockfile has no packages (run `npm install` first)")
	}
	return lf, nil
}

// InstallStore 版本化 store 布局：<root>/<modulesDir>/.store/<name>@<ver>/ + manifest.json。
// 同一包的多个版本会分别落盘，manifest 记录"哪个包该用哪个版本"。
func InstallStore(root, modulesDir string) error {
	lf, err := readLock(root)
	if err != nil {
		return err
	}
	if modulesDir == "" {
		modulesDir = "node_modules"
	}
	storeDir := filepath.Join(root, modulesDir, ".store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return err
	}

	// 1) 收集唯一 name@version → 下载源
	type src struct{ resolved, integrity string }
	uniq := map[string]src{}
	for key, p := range lf.Packages {
		if key == "" || p.Version == "" || p.Resolved == "" || p.Link {
			continue
		}
		name := pkgNameFromKey(key)
		if name == "" {
			continue
		}
		uniq[StoreDirName(name, p.Version)] = src{p.Resolved, p.Integrity}
	}

	client := &http.Client{Timeout: 60 * time.Second}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		done     int
		skipped  int
		firstErr error
	)
	sem := make(chan struct{}, 16)

	for dirName, s := range uniq {
		dest := filepath.Join(storeDir, dirName)
		if _, err := os.Stat(filepath.Join(dest, "package.json")); err == nil {
			mu.Lock()
			skipped++
			mu.Unlock()
			continue
		}

		mu.Lock()
		if firstErr != nil {
			mu.Unlock()
			break
		}
		mu.Unlock()

		wg.Add(1)
		sem <- struct{}{}
		go func(dirName string, s src, dest string) {
			defer wg.Done()
			defer func() { <-sem }()

			mu.Lock()
			if firstErr != nil {
				mu.Unlock()
				return
			}
			mu.Unlock()

			if err := downloadAndExtract(client, s.resolved, s.integrity, dest); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("pkg %s: %w", dirName, err)
				}
				mu.Unlock()
				return
			}

			mu.Lock()
			done++
			mu.Unlock()
		}(dirName, s, dest)
	}

	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	// 2) 生成 manifest：root 依赖 + 每个包的依赖版本（向上查找确定可见版本）
	m := StoreManifest{
		Root:     map[string]string{},
		Packages: map[string]map[string]string{},
	}
	if rootPkg, ok := lf.Packages[""]; ok {
		for dep := range mergeDeps(rootPkg.Dependencies, rootPkg.DevDependencies) {
			if v, ok := visibleVersion(lf.Packages, "", dep); ok {
				m.Root[dep] = v
			}
		}
	}
	for key, p := range lf.Packages {
		if key == "" || p.Version == "" {
			continue
		}
		name := pkgNameFromKey(key)
		if name == "" {
			continue
		}
		deps := mergeDeps(p.Dependencies, p.OptionalDependencies)
		if len(deps) == 0 {
			continue
		}
		out := map[string]string{}
		for dep := range deps {
			if v, ok := visibleVersion(lf.Packages, key, dep); ok {
				out[dep] = v
			}
		}
		if len(out) > 0 {
			m.Packages[StoreDirName(name, p.Version)] = out
		}
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(storeDir, "manifest.json"), append(mb, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("✅ wf install(store): %d 包入 store, %d 已存在, manifest 覆盖 %d 个包\n",
		done, skipped, len(m.Packages))
	return nil
}

func mergeDeps(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// pkgNameFromKey：lockfile 键 → 包名（最后一个 "node_modules/" 之后）。
func pkgNameFromKey(key string) string {
	const marker = "node_modules/"
	idx := strings.LastIndex(key, marker)
	if idx < 0 {
		return ""
	}
	return key[idx+len(marker):]
}

// visibleVersion：从 pkgPath 出发"向上查找"依赖 dep 的可见版本（npm 解析语义）。
func visibleVersion(pkgs map[string]lockPackage, pkgPath, dep string) (string, bool) {
	cur := pkgPath
	for {
		cand := "node_modules/" + dep
		if cur != "" {
			cand = cur + "/node_modules/" + dep
		}
		if p, ok := pkgs[cand]; ok && p.Version != "" {
			return p.Version, true
		}
		if cur == "" {
			break
		}
		if i := strings.LastIndex(cur, "/node_modules/"); i >= 0 {
			cur = cur[:i]
		} else if strings.HasPrefix(cur, "node_modules/") {
			cur = ""
		} else {
			cur = ""
		}
	}
	return "", false
}

func downloadAndExtract(c *http.Client, url, integrity, dest string) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := verifyIntegrity(body, integrity); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return extractTarball(body, dest)
}

// verifyIntegrity 校验 npm 的 "sha512-<base64>" 完整性声明。
func verifyIntegrity(body []byte, integrity string) error {
	if integrity == "" {
		return nil
	}
	parts := strings.SplitN(integrity, "-", 2)
	if len(parts) != 2 {
		return fmt.Errorf("unsupported integrity format: %q", integrity)
	}
	switch parts[0] {
	case "sha512":
		sum := sha512.Sum512(body)
		got := base64.StdEncoding.EncodeToString(sum[:])
		if got != parts[1] {
			return fmt.Errorf("integrity mismatch: expected %s, got %s", parts[1], got)
		}
	default:
		return fmt.Errorf("unsupported hash algo: %q", parts[0])
	}
	return nil
}

// extractTarball 解压 npm tarball（gzip + tar，顶层 "package/" 需剥离）。
func extractTarball(body []byte, dest string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "package/")
		if name == "" {
			continue
		}
		out := filepath.Join(dest, name)
		if hdr.FileInfo().IsDir() {
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return nil
}
