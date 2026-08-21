// BetterOCR 是一个多引擎 OCR 融合工具:并发调用多个便宜的 VLM 引擎,
// 将完整文本动态切成中文句段,只把争议句段交给更强的 VLM 仲裁。
// 强模型成本与分歧量成正比,融合准确率可超过任何单个引擎。
//
// 全部运行参数来自 JSON 配置文件(模板内置在二进制中,启动时自动
// 释放/补全),不读取任何环境变量。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lieyanc/BetterOCR/internal/config"
	"github.com/lieyanc/BetterOCR/internal/database"
	"github.com/lieyanc/BetterOCR/internal/model"
	"github.com/lieyanc/BetterOCR/internal/pipeline"
	"github.com/lieyanc/BetterOCR/internal/server"
)

func main() {
	var (
		configPath   = flag.String("config", config.DefaultPath, "JSON 配置文件路径;不存在时自动释放内置模板,缺字段时按模板补全写回")
		databasePath = flag.String("db", database.DefaultPath, "Web 模式的 JSON 数据库路径(用户、会话、任务与可编辑设置)")
		serveMode    = flag.Bool("serve", false, "以 Web 模式启动,监听配置中的 serve_addr;不带图片参数时也是 Web 模式,此旗标可省略")
		pretty       = flag.Bool("pretty", false, "美化 JSON 输出(CLI 模式)")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"用法: betterocr [-config data/config.json] image.png\n"+
				"      betterocr [-config data/config.json] [-serve]  # 不带图片参数时默认进入 Web 模式\n\n"+
				"Provider、模型目录、默认选择、超时与监听地址全部来自 JSON 配置文件;\n"+
				"首次运行自动生成模板,不读取任何环境变量。\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *configPath == config.DefaultPath {
		if moved, err := migrateLegacyFile("betterocr.json", config.DefaultPath); err != nil {
			fatal("迁移旧配置文件失败:", err)
		} else if moved {
			fmt.Fprintf(os.Stderr, "已迁移旧配置文件到 %s\n", config.DefaultPath)
		}
	}

	cfg, action, err := config.Load(*configPath)
	if err != nil {
		fatal("加载配置失败:", err)
	}
	switch action {
	case config.ActionReleased:
		fmt.Fprintf(os.Stderr, "已生成默认配置文件 %s(内置模板),请按需修改 providers、engines 等字段\n", *configPath)
		if !*serveMode && flag.NArg() > 0 {
			// 模板值直接识别几乎必然失败,提示编辑后退出;Web 模式仍可启动,
			// 但页面只能点选模型,密钥等仍需编辑配置文件后重启生效
			fmt.Fprintln(os.Stderr, "请编辑配置后重新运行")
			os.Exit(1)
		}
	case config.ActionSupplemented:
		fmt.Fprintf(os.Stderr, "配置文件 %s 缺少字段,已按内置模板补全\n", *configPath)
	}

	// 默认 Web 模式:不带任何位置参数时进入 Web 模式,-serve 显式声明效果相同
	if *serveMode || flag.NArg() == 0 {
		runServe(cfg, *databasePath)
		return
	}
	image, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fatal("读取图片失败:", err)
	}
	if cfg.Arbiter == "" {
		fmt.Fprintln(os.Stderr, "警告: 配置中 arbiter 为空,争议句段将使用本地候选;Web 模式下仍可人工合并")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout())
	defer cancel()
	engines, err := cfg.ResolveMany(cfg.Engines)
	if err != nil {
		fatal("配置错误:", err)
	}
	var arbiterModel *model.Resolved
	if cfg.Arbiter != "" {
		resolved, resolveErr := cfg.Resolve(cfg.Arbiter)
		if resolveErr != nil {
			fatal("配置错误:", resolveErr)
		}
		arbiterModel = &resolved
	}

	final, err := pipeline.Run(ctx, pipeline.Config{
		Engines: engines,
		Arbiter: arbiterModel,
	}, image)
	if err != nil {
		fatal("识别失败:", err)
	}

	var out []byte
	if *pretty {
		out, err = json.MarshalIndent(final, "", "  ")
	} else {
		out, err = json.Marshal(final)
	}
	if err != nil {
		fatal("序列化结果失败:", err)
	}
	fmt.Println(string(out))

	if final.Stats.Engines > 0 && final.Stats.FailedEngines == final.Stats.Engines {
		fmt.Fprintln(os.Stderr, "所有引擎均失败,详见输出中的 candidates[].err")
		os.Exit(1)
	}
}

// runServe 启动 Web 模式:内嵌前端 + /api 接口,配置文件的值作为页面默认值。
func runServe(cfg config.Config, databasePath string) {
	if databasePath == database.DefaultPath {
		if moved, err := migrateLegacyFile("betterocr.db.json", database.DefaultPath); err != nil {
			fatal("迁移旧 JSON 数据库失败:", err)
		} else if moved {
			fmt.Fprintf(os.Stderr, "已迁移旧 JSON 数据库到 %s\n", database.DefaultPath)
		}
	}
	store, err := database.Open(databasePath, cfg)
	if err != nil {
		fatal("打开 JSON 数据库失败:", err)
	}
	cfg = store.Settings()
	if cfg.ServeAddr == "" {
		fatal("配置错误:", "serve_addr 为空,Web 模式无监听地址")
	}
	srv := &server.Server{
		Config:  cfg,
		Timeout: cfg.Timeout(),
		Store:   store,
	}
	fmt.Fprintf(os.Stderr, "BetterOCR Web 模式已启动: http://%s\n", displayAddr(cfg.ServeAddr))
	hs := &http.Server{Addr: cfg.ServeAddr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := hs.ListenAndServe(); err != nil {
		fatal("HTTP 服务失败:", err)
	}
}

func migrateLegacyFile(legacyPath, targetPath string) (bool, error) {
	if _, err := os.Stat(targetPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if _, err := os.Stat(legacyPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return false, err
	}
	if err := os.Rename(legacyPath, targetPath); err != nil {
		return false, err
	}
	if err := os.Chmod(targetPath, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// displayAddr 把 ":8787" 这类监听地址渲染成可点击的 URL 主机部分。
func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

func fatal(msg string, v any) {
	fmt.Fprintln(os.Stderr, msg, v)
	os.Exit(1)
}
