package compiler

import (
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// BuildProfile is the explicit description of one build target. It replaces
// the old "production is the only mode" assumption and lets dev/SSR/client
// builds share one option builder.
//
// Zero values are filled in from Mode: production gets NODE_ENV=production,
// development gets NODE_ENV=development. Define entries passed by the caller
// win over the defaults.
type BuildProfile struct {
	Mode string // "development" | "production"

	Define     map[string]string
	Target     api.Target
	Minify     bool
	SourceMap  bool
	Loaders    map[string]api.Loader
	PublicPath string
	Conditions []string

	JSX             api.JSX
	JSXFactory      string
	JSXFragment     string
	JSXImportSource string
}

// ProductionProfile is the historical build behavior: production defines,
// no source maps and no minification. Client bundles opt into minification
// explicitly in their wrappers.
func ProductionProfile() BuildProfile {
	return BuildProfile{Mode: "production"}
}

// DevelopmentProfile is intended for dev SSR builds and tests: development
// defines, inline source maps and modern syntax.
func DevelopmentProfile() BuildProfile {
	return BuildProfile{
		Mode:      "development",
		Target:    api.ESNext,
		SourceMap: true,
	}
}

func (p BuildProfile) resolve(defaultMode string, defaultTarget api.Target) BuildProfile {
	if p.Mode == "" {
		p.Mode = defaultMode
	}
	if p.Target == 0 {
		p.Target = defaultTarget
	}
	def := map[string]string{}
	if p.Mode == "development" {
		def["process.env.NODE_ENV"] = `"development"`
	} else {
		def["process.env.NODE_ENV"] = `"production"`
	}
	for k, v := range p.Define {
		def[k] = v
	}
	p.Define = def
	return p
}

// assetExtensions are the file types treated as static assets. Client builds
// emit them with a content hash; SSR builds inline them as data URLs because
// the goja/SSR pipeline has no asset URL host.
var assetExtensions = []string{
	".svg", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".ico", ".bmp",
	".woff", ".woff2", ".ttf", ".otf", ".eot",
	".mp4", ".webm", ".mp3", ".wav", ".ogg", ".wasm",
}

// IsAssetExt reports whether ext is handled by the static-asset loaders.
// dev 的 /@wf/asset 路由与 build 的 LoaderFile 必须用同一张表。
func IsAssetExt(ext string) bool {
	if ext == "" {
		return false
	}
	if ext[0] != '.' {
		ext = "." + ext
	}
	for _, e := range assetExtensions {
		if strings.EqualFold(e, ext) {
			return true
		}
	}
	return false
}

func (p BuildProfile) loaderMap(ssr bool) map[string]api.Loader {
	out := make(map[string]api.Loader, len(assetExtensions)+len(p.Loaders))
	for _, ext := range assetExtensions {
		if ssr {
			out[ext] = api.LoaderDataURL
		} else {
			out[ext] = api.LoaderFile
		}
	}
	for ext, loader := range p.Loaders {
		if ext == "" {
			continue
		}
		ext = strings.ToLower(ext)
		if ext[0] != '.' {
			ext = "." + ext
		}
		out[ext] = loader
	}
	return out
}

func (p BuildProfile) sourceMap() api.SourceMap {
	if p.SourceMap {
		return api.SourceMapInline
	}
	return api.SourceMapNone
}

func firstJSOutput(files []api.OutputFile) []byte {
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".js") || strings.HasSuffix(f.Path, ".mjs") {
			return f.Contents
		}
	}
	if len(files) > 0 {
		return files[0].Contents
	}
	return nil
}
