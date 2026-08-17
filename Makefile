.PHONY: build web run test clean

# 完整构建:前端 → go:embed → 单文件二进制 ./betterocr
build: web
	go build -o betterocr ./cmd/betterocr

# 仅构建前端(产物在 web/dist,由 go build 内嵌)
web:
	cd web && npm install && npm run build

# 编译并运行
run: build
	./betterocr

test:
	go test ./...

clean:
	rm -f betterocr
	find web/dist -mindepth 1 -not -name '.gitkeep' -delete
