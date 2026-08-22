package server

import (
	"net/http"

	"github.com/lieyanc/BetterOCR/internal/updater"
	"github.com/lieyanc/BetterOCR/internal/version"
)

// versionResponse reports build identity plus the OTA settings in force. It is
// the only unauthenticated endpoint here: knowing which build answers is useful
// for probes, while every state-changing update action needs an admin session.
type versionResponse struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	BuildTime     string `json:"build_time"`
	UpdateEnabled bool   `json:"update_enabled"`
	UpdateChannel string `json:"update_channel"`
	UpdateSource  string `json:"update_source"`
	UpdateRepo    string `json:"update_repo"`
}

// updateCheckResponse carries a failed check as a field rather than an HTTP
// error so the admin UI can show it beside the current status.
type updateCheckResponse struct {
	updater.CheckResult
	Error string `json:"error,omitempty"`
}

// UpdateConfig returns the live update block. The updater calls it before every
// use, so settings saved through the admin API apply without a restart.
func (s *Server) UpdateConfig() updater.Config {
	return updater.Normalize(s.currentConfig().Update)
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	update := s.UpdateConfig()
	writeJSON(w, http.StatusOK, versionResponse{
		Version:       version.Version,
		Commit:        version.Commit,
		BuildTime:     version.BuildTime,
		UpdateEnabled: update.Enabled,
		UpdateChannel: update.Channel,
		UpdateSource:  update.Source,
		UpdateRepo:    update.Repo,
	})
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, _ *http.Request) {
	if !s.updaterReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.Updater.Status())
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.updaterReady(w) {
		return
	}
	result, err := s.Updater.CheckOnly(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, updateCheckResponse{CheckResult: result, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, updateCheckResponse{CheckResult: result})
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.updaterReady(w) {
		return
	}
	// ready 表示预发布二进制已下载校验完毕,此时 apply 只是重启;
	// 其余状态走完整的检查 + 下载 + 安装。
	switch status := s.Updater.Status(); status.State {
	case "ready":
		if err := s.Updater.ApplyPending(r.Context()); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "applying"})
	case "checking", "downloading", "applying":
		writeErr(w, http.StatusConflict, "更新正在进行中")
	default:
		s.Updater.StartUpdate(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"status": "update_started"})
	}
}

func (s *Server) handleUpdateDismiss(w http.ResponseWriter, _ *http.Request) {
	if !s.updaterReady(w) {
		return
	}
	s.Updater.DismissPending()
	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

func (s *Server) updaterReady(w http.ResponseWriter) bool {
	if s.Updater == nil {
		writeErr(w, http.StatusServiceUnavailable, "当前进程未启用自更新")
		return false
	}
	return true
}
