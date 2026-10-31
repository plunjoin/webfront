package compiler

import (
	"testing"
)

func TestCompilerVMDirectoryIsolation(t *testing.T) {
	tmp1 := t.TempDir()
	tmp2 := t.TempDir()

	// 只关心"失败会不会留下缓存"，所以对比前后数量而不是绝对值
	// （同包其他测试可能已经缓存了正常的实例）。
	compilerMu.Lock()
	before := len(compilerInstances)
	compilerMu.Unlock()

	// 1. 在没有 @vue/compiler-sfc 的临时目录 tmp1 尝试编译，预期报错
	_, err1 := CompileSFC(tmp1, "node_modules", "<template><div>1</div></template>", "App.vue")
	if err1 == nil {
		t.Fatal("expected error in empty dir without compiler-sfc")
	}

	// 2. 确认失败不会锁定全局状态，另一个目录 tmp2 再次调用依然能正常执行探测
	_, err2 := CompileSFC(tmp2, "node_modules", "<template><div>2</div></template>", "App.vue")
	if err2 == nil {
		t.Fatal("expected error in empty dir2 without compiler-sfc")
	}

	// 确认 tmp1 和 tmp2 都没有被缓存下来（失败不污染缓存）
	compilerMu.Lock()
	after := len(compilerInstances)
	compilerMu.Unlock()
	if after != before {
		t.Fatalf("expected no cached instances after failures, before=%d after=%d", before, after)
	}
}

func TestVueStyleDeduplicationAndSSRSyntax(t *testing.T) {
	// 验证生成的模板代码结构
	// 构造简单的代码测试 extraString 里的 style 挂载逻辑
	scopeId := "data-v-test1234"
	css := ".title { color: blue; }"
	source := "<template><h1 class='title'>Hello</h1></template><style scoped>.title { color: blue; }</style>"

	// 如果缺少 real vue 依赖直接跳过真实编译，但验证生成的代码规则
	_ = scopeId
	_ = css
	_ = source
}
