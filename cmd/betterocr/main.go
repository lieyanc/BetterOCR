// BetterOCR 是一个多引擎 OCR 融合工具:并发调用多个便宜的 VLM 引擎,
// 行级对齐后一致的行免费通过,只有分歧行交给更强的 VLM 仲裁。
// 强模型成本与分歧量成正比,融合准确率可超过任何单个引擎。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lieyanc/BetterOCR/internal/pipeline"
	"github.com/lieyanc/BetterOCR/internal/server"
)

func main() {
	var (
		engines   = flag.String("engines", "", "基础 VLM 引擎模型,逗号分隔;同一模型重复出现即多路采样(CLI 模式必填,Web 模式作为页面默认值)")
		arbModel  = flag.String("arbiter", "", "仲裁 VLM 模型(应显著强于基础引擎);留空则分歧行退化为本地择优")
		baseURL   = flag.String("base-url", "", "OpenAI 兼容 API 地址;默认依次取 $OPENAI_BASE_URL、$OPENAI_API_BASE,最后 https://api.openai.com/v1")
		timeout   = flag.Duration("timeout", 2*time.Minute, "单次识别的端到端超时(含引擎识别与仲裁)")
		pretty    = flag.Bool("pretty", false, "美化 JSON 输出(CLI 模式)")
		serveAddr = flag.String("serve", "", "以 Web 模式启动并监听该地址,如 127.0.0.1:8787;前端已内嵌在二进制中")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"用法: betterocr -engines <models> [-arbiter <model>] [flags] image.png\n"+
				"      betterocr -serve 127.0.0.1:8787 [-engines <models>] [-arbiter <model>]\n\n"+
				"API Key 从 $OPENAI_API_KEY 读取(本地服务可不设)。\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *serveAddr != "" {
		runServe(*serveAddr, *engines, *arbModel, *baseURL, *timeout)
		return
	}

	if *engines == "" || flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	image, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fatal("读取图片失败:", err)
	}
	if *arbModel == "" {
		fmt.Fprintln(os.Stderr, "警告: 未配置 -arbiter,分歧行将退化为本地择优,无法获得超越单引擎的准确率")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	final, err := pipeline.Run(ctx, pipeline.Config{
		Engines: pipeline.SplitList(*engines),
		Arbiter: *arbModel,
		BaseURL: pipeline.ResolveBaseURL(*baseURL),
		APIKey:  os.Getenv("OPENAI_API_KEY"),
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

// runServe 启动 Web 模式:内嵌前端 + /api 接口,CLI 参数作为页面默认值。
func runServe(addr, engines, arbModel, baseURL string, timeout time.Duration) {
	srv := &server.Server{
		DefaultEngines: pipeline.SplitList(engines),
		DefaultArbiter: arbModel,
		BaseURL:        pipeline.ResolveBaseURL(baseURL),
		APIKey:         os.Getenv("OPENAI_API_KEY"),
		Timeout:        timeout,
	}
	fmt.Fprintf(os.Stderr, "BetterOCR Web 模式已启动: http://%s\n", displayAddr(addr))
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := hs.ListenAndServe(); err != nil {
		fatal("HTTP 服务失败:", err)
	}
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
