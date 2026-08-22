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
	"errors"
	"flag"
	"fmt"
	"log"
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
	"github.com/lieyanc/BetterOCR/internal/updater"
	"github.com/lieyanc/BetterOCR/internal/version"
)

func main() {
	var (
		configPath   = flag.String("config", config.DefaultPath, "JSON 配置文件路径;不存在时自动释放内置模板,缺字段时按模板补全写回")
		databasePath = flag.String("db", database.DefaultPath, "Web 模式的 JSON 数据库路径(用户、会话、任务与文档元数据)")
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
		runServe(cfg, *configPath, *databasePath)
		return
	}
	image, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fatal("读取图片失败:", err)
	}
	if cfg.Arbiter == "" {
		fmt.Fprintln(os.Stderr, "警告: 配置中 arbiter 为空,争议句段将使用本地候选;Web 模式下仍可人工合并")
	}

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
	var duplicateChecker *model.Resolved
	if cfg.DuplicateChecker != "" {
		resolved, resolveErr := cfg.Resolve(cfg.DuplicateChecker)
		if resolveErr != nil {
			fatal("配置错误:", resolveErr)
		}
		duplicateChecker = &resolved
	}

	final, err := pipeline.Run(context.Background(), pipeline.Config{
		Engines:            engines,
		Arbiter:            arbiterModel,
		DuplicateChecker:   duplicateChecker,
		EngineTimeout:      cfg.EngineTimeout(),
		ArbiterTimeout:     cfg.ArbiterTimeout(),
		EngineMaxAttempts:  cfg.EngineAttempts(),
		ArbiterMaxAttempts: cfg.ArbiterAttempts(),
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

// runServe 启动 Web 模式。CLI 与 Web 始终共享同一个配置文件。
func runServe(cfg config.Config, configPath, databasePath string) {
	if databasePath == database.DefaultPath {
		if moved, err := migrateLegacyFile("betterocr.db.json", database.DefaultPath); err != nil {
			fatal("迁移旧 JSON 数据库失败:", err)
		} else if moved {
			fmt.Fprintf(os.Stderr, "已迁移旧 JSON 数据库到 %s\n", database.DefaultPath)
		}
	}
	store, err := database.Open(databasePath)
	if err != nil {
		fatal("打开 JSON 数据库失败:", err)
	}
	cfg, migrated, err := migrateLegacyWebSettings(store, configPath, cfg)
	if err != nil {
		fatal("迁移旧 Web 配置失败:", err)
	}
	if migrated {
		fmt.Fprintf(os.Stderr, "已将旧 Web 配置迁移到 %s，并从数据库移除 settings\n", configPath)
	}
	if cfg.ServeAddr == "" {
		fatal("配置错误:", "serve_addr 为空,Web 模式无监听地址")
	}
	// 上一次进程(可能是自更新重启)留下的僵尸任务必须在这里收尾,
	// 否则它们永远停在 running。文档项目的恢复在 srv.Prepare 里。
	if recovered, err := store.FailInterruptedTasks(); err != nil {
		fatal("恢复中断的任务记录失败:", err)
	} else if recovered > 0 {
		fmt.Fprintf(os.Stderr, "已将 %d 个被重启中断的任务标记为失败\n", recovered)
	}
	srv := &server.Server{
		Config:     cfg,
		ConfigPath: configPath,
		Store:      store,
	}
	// 文档后台在这里就绪:上一次进程留下的未完成页面会重新排队。初始化失败
	// 只让文档接口降级为 503,单图识别与其余功能照常可用。
	if err := srv.Prepare(); err != nil {
		fmt.Fprintln(os.Stderr, "警告: 文档后台不可用:", err)
	}

	bgCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	hs := &http.Server{Addr: cfg.ServeAddr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	restartErr := make(chan error, 1)
	srv.Updater = updater.New(
		srv.UpdateConfig,
		func() string { return store.Directory() },
		log.New(os.Stderr, "", log.LstdFlags),
		updater.RestartHooks{
			BeforeExec: func(tag string) error {
				// 停后台检查并优雅关闭监听,再交出进程镜像。
				cancelBackground()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				if err := hs.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				fmt.Fprintf(os.Stderr, "已为更新 %s 准备重启\n", tag)
				return nil
			},
			OnExecFailure: func(err error) {
				select {
				case restartErr <- err:
				default:
				}
			},
			IsBusy: srv.HasActiveJobs,
		},
	)

	fmt.Fprintf(os.Stderr, "BetterOCR %s (commit=%s, built=%s)\n", version.Version, version.Commit, version.BuildTime)
	fmt.Fprintf(os.Stderr, "BetterOCR Web 模式已启动: http://%s\n", displayAddr(cfg.ServeAddr))
	srv.Updater.StartBackground(bgCtx)

	err = hs.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		// 只有 BeforeExec 会主动关闭监听:此时等 exec 的结果,
		// 成功的话进程镜像已被替换,永远走不到这里。
		if execErr := <-restartErr; execErr != nil {
			fatal("自更新重启失败:", execErr)
		}
		return
	}
	fatal("HTTP 服务失败:", err)
}

func migrateLegacyWebSettings(store *database.Store, configPath string, current config.Config) (config.Config, bool, error) {
	raw := store.LegacySettings()
	if len(raw) == 0 {
		return current, false, nil
	}
	var legacy config.Config
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return current, false, err
	}
	if strings.TrimSpace(legacy.ServeAddr) == "" {
		return current, false, errors.New("serve_addr 为空")
	}
	if err := config.Save(configPath, legacy); err != nil {
		return current, false, err
	}
	legacy, _, err := config.Load(configPath)
	if err != nil {
		return current, false, err
	}
	if err := store.DiscardLegacySettings(); err != nil {
		return current, false, err
	}
	return legacy, true, nil
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
