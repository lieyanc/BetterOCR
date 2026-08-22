package updater

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

const workflowPath = "../../.github/workflows/cross-compile.yml"

type matrixTarget struct {
	target string
	goos   string
	goarch string
	ext    string
}

// TestReleaseAssetNamesMatchCIMatrix 把 CI 构建矩阵与 targetName() 对表。
// 资产命名协议 = 矩阵的 target 字段 × updater 的 targetName();历史上
// 上游项目正是因为两边不一致(updater 映射了一个矩阵里没有的 target),
// 导致该平台自更新永久 404。任何一边改动都必须让这个测试继续通过。
func TestReleaseAssetNamesMatchCIMatrix(t *testing.T) {
	targets := parseMatrix(t)
	if len(targets) == 0 {
		t.Fatalf("no build targets parsed from %s", workflowPath)
	}

	for _, entry := range targets {
		if want := entry.goos + "-" + entry.goarch; entry.target != want {
			t.Errorf("matrix target %q does not match goos/goarch %q", entry.target, want)
		}
		wantExt := ""
		if entry.goos == "windows" {
			wantExt = ".exe"
		}
		if entry.ext != wantExt {
			t.Errorf("matrix target %q has ext %q, want %q", entry.target, entry.ext, wantExt)
		}
	}

	// 当前平台若在矩阵里,它请求的资产名必须逐字存在于发布集合中。
	local := (&Updater{}).targetName()
	for _, entry := range targets {
		if entry.goos == runtime.GOOS && entry.goarch == runtime.GOARCH {
			if asset := "betterocr-" + entry.target + entry.ext; asset != local {
				t.Fatalf("targetName() = %q but CI publishes %q", local, asset)
			}
			return
		}
	}
	t.Logf("current platform %s/%s is not in the release matrix; skipping asset name check",
		runtime.GOOS, runtime.GOARCH)
}

// parseMatrix reads the target/goos/goarch/ext quadruples out of the workflow.
func parseMatrix(t *testing.T) []matrixTarget {
	t.Helper()
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var (
		targets []matrixTarget
		current *matrixTarget
	)
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := splitYAMLField(line)
		if !ok {
			continue
		}
		switch key {
		case "- target":
			targets = append(targets, matrixTarget{target: value})
			current = &targets[len(targets)-1]
		case "goos":
			if current != nil {
				current.goos = value
			}
		case "goarch":
			if current != nil {
				current.goarch = value
			}
		case "ext":
			if current != nil {
				current.ext = value
				current = nil
			}
		}
	}
	return targets
}

func splitYAMLField(line string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(strings.TrimSpace(line), ":")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"`), true
}
