package jsruntime

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPoolNormalRender(t *testing.T) {
	bundle := []byte(`
globalThis.__WF_APP__ = {
  render: function(url, props) {
    return {
      html: "<div>Hello " + (props && props.name || "World") + " at " + url + "</div>",
      head: "<title>SSR Title</title>"
    };
  }
};
`)
	pool, err := New(bundle, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	html, head, err := pool.RenderHTML("/home", []byte(`{"name":"Webfront"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "Hello Webfront at /home") {
		t.Fatalf("unexpected html: %s", html)
	}
	if head != "<title>SSR Title</title>" {
		t.Fatalf("unexpected head: %s", head)
	}
}

func TestPoolTimeoutAndRecovery(t *testing.T) {
	// 构造一个在 /infinite 时死循环、在 /normal 时正常渲染的 bundle
	bundle := []byte(`
globalThis.__WF_APP__ = {
  render: function(url, props) {
    if (url === '/infinite') {
      while(true) {}
    }
    return {
      html: "<p>Healthy Render</p>",
      head: ""
    };
  }
};
`)
	// 设置极短超时 100ms
	pool, err := NewWithTimeout(bundle, 1, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// 1. 触发超时死循环
	_, _, err = pool.RenderHTML("/infinite", []byte(`{}`))
	if err == nil {
		t.Fatal("expected timeout error for infinite loop, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected timeout error message, got: %v", err)
	}

	// 2. 验证坏 VM 被销毁并重建，后续正常请求不受影响
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	html, _, err := pool.RenderHTMLContext(ctx, "/normal", []byte(`{}`))
	if err != nil {
		t.Fatalf("expected healthy render after timeout recovery, got: %v", err)
	}
	if !strings.Contains(html, "Healthy Render") {
		t.Fatalf("unexpected html: %s", html)
	}
}

func TestPoolSSRStylesFromModuleLoad(t *testing.T) {
	// 真实 SFC 的样式是在模块求值时 push 的（早于任何一次 render），
	// 不能被 renderScript 的 per-render 重置清掉，也不能跨请求重复。
	bundle := []byte(`
globalThis.__WF_STYLES__.push('.sfc { color: red; }');
globalThis.__WF_APP__ = {
  render: function(url, props) {
    return { html: '<div class="sfc"></div>', head: '' };
  }
};
`)
	pool, err := New(bundle, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	for i := 0; i < 2; i++ {
		_, head, err := pool.RenderHTML("/", []byte(`{}`))
		if err != nil {
			t.Fatalf("render #%d: %v", i, err)
		}
		if n := strings.Count(head, "<style>"); n != 1 {
			t.Fatalf("render #%d: 期望恰好 1 个 <style>，实际 %d: %s", i, n, head)
		}
		if !strings.Contains(head, ".sfc { color: red; }") {
			t.Fatalf("render #%d: 模块加载期的样式丢失: %s", i, head)
		}
	}
}

func TestPoolSSRStylesCollection(t *testing.T) {
	bundle := []byte(`
globalThis.__WF_APP__ = {
  render: function(url, props) {
    if (typeof globalThis.__WF_STYLES__ !== 'undefined') {
      globalThis.__WF_STYLES__.push('.vue-btn { color: red; }');
      globalThis.__WF_STYLES__.push('.vue-card { border: 1px solid blue; }');
    }
    return {
      html: "<button class='vue-btn'>Click</button>",
      head: "<title>Vue SSR Head</title>"
    };
  }
};
`)
	pool, err := New(bundle, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	html, head, err := pool.RenderHTML("/", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "vue-btn") {
		t.Fatalf("unexpected html: %s", html)
	}
	if !strings.Contains(head, "<style>.vue-btn { color: red; }</style>") ||
		!strings.Contains(head, "<style>.vue-card { border: 1px solid blue; }</style>") ||
		!strings.Contains(head, "<title>Vue SSR Head</title>") {
		t.Fatalf("expected collected styles in head, got: %s", head)
	}
}
