.PHONY: build web run test clean

# 版本信息随构建注入,/api/version 与自更新的版本比较都读它;
# CI 会用同样的 -X 注入 dev-{run}-{date}-{sha} 或 vX.Y.Z。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
VERSION_PKG := github.com/lieyanc/BetterOCR/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).BuildTime=$(BUILD_TIME)

# 完整构建:前端 → go:embed → 单文件二进制 ./betterocr
build: web
	go build -ldflags="$(LDFLAGS)" -o betterocr ./cmd/betterocr

# 仅构建前端(产物在 web/dist,由 go build 内嵌)
web:
	cd web && npm install && npm run build

# 编译并运行
run: build
	./betterocr

test:
	go test ./...

clean:
	rm -f betterocr betterocr.bak
	find web/dist -mindepth 1 -not -name '.gitkeep' -delete
