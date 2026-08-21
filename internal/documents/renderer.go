// Package documents provides bounded, server-side PDF page rendering.
package documents

import (
	"context"
	"fmt"
	"image"
	"os"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

const renderLongEdge = 2200

// PageRenderer renders one PDF at a time and never retains completed pages.
type PageRenderer interface {
	Render(
		ctx context.Context,
		path string,
		onCount func(pageCount int) error,
		shouldRender func(pageIndex int) bool,
		onPage func(pageIndex int, page image.Image) error,
	) error
	Close() error
}

// PDFiumRenderer uses the PDFium WebAssembly build embedded by go-pdfium. The
// worker limit is intentionally one so large documents have a predictable
// memory ceiling even when several users upload at the same time.
type PDFiumRenderer struct {
	pool pdfium.Pool
}

func NewPDFiumRenderer(ctx context.Context) (*PDFiumRenderer, error) {
	pool, err := webassembly.Init(webassembly.Config{
		Context:      ctx,
		MinIdle:      0,
		MaxIdle:      0,
		MaxTotal:     1,
		ReuseWorkers: false,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 PDF 渲染器失败: %w", err)
	}
	return &PDFiumRenderer{pool: pool}, nil
}

func (r *PDFiumRenderer) Close() error {
	if r == nil || r.pool == nil {
		return nil
	}
	return r.pool.Close()
}

func (r *PDFiumRenderer) Render(
	ctx context.Context,
	path string,
	onCount func(pageCount int) error,
	shouldRender func(pageIndex int) bool,
	onPage func(pageIndex int, page image.Image) error,
) error {
	instance, err := r.pool.GetInstanceWithContext(ctx)
	if err != nil {
		return fmt.Errorf("获取 PDF 渲染工作器失败: %w", err)
	}
	defer instance.Close()

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	document, err := instance.OpenDocument(&requests.OpenDocument{
		FileReader: file, FileReaderSize: stat.Size(),
	})
	if err != nil {
		return fmt.Errorf("打开 PDF 失败: %w", err)
	}
	defer instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: document.Document})

	countResponse, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: document.Document})
	if err != nil {
		return fmt.Errorf("读取 PDF 页数失败: %w", err)
	}
	if countResponse.PageCount < 1 {
		return fmt.Errorf("PDF 没有可处理的页面")
	}
	if err := onCount(countResponse.PageCount); err != nil {
		return err
	}

	for pageIndex := 0; pageIndex < countResponse.PageCount; pageIndex++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if shouldRender != nil && !shouldRender(pageIndex) {
			continue
		}
		response, err := instance.RenderPageInPixels(&requests.RenderPageInPixels{
			Width: renderLongEdge, Height: renderLongEdge,
			Page: requests.Page{ByIndex: &requests.PageByIndex{
				Document: document.Document, Index: pageIndex,
			}},
			Document:   &document.Document,
			RenderForm: true,
		})
		if err != nil {
			return fmt.Errorf("渲染第 %d 页失败: %w", pageIndex+1, err)
		}
		err = onPage(pageIndex, response.Result.RenderedImage)
		response.Cleanup()
		if err != nil {
			return err
		}
	}
	return nil
}
