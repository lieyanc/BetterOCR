package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/config"
	"github.com/lieyanc/BetterOCR/internal/database"
	"github.com/lieyanc/BetterOCR/internal/pipeline"
)

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusOK, []database.Task{})
		return
	}
	auth, _ := authFromRequest(r)
	writeJSON(w, http.StatusOK, s.Store.Tasks(auth.User.ID, auth.User.Role == database.RoleAdmin))
}

func (s *Server) handleUsers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Store.Users())
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username string        `json:"username"`
		Password string        `json:"password"`
		Role     database.Role `json:"role"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := s.Store.CreateUser(request.Username, request.Password, request.Role)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username string         `json:"username"`
		Password string         `json:"password"`
		Role     *database.Role `json:"role"`
		Disabled *bool          `json:"disabled"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, _ := authFromRequest(r)
	id := r.PathValue("id")
	if id == auth.User.ID && (request.Disabled != nil && *request.Disabled || request.Role != nil && *request.Role != database.RoleAdmin) {
		writeErr(w, http.StatusBadRequest, "不能停用或降级当前登录用户")
		return
	}
	user, err := s.Store.UpdateUser(id, database.UserUpdate{
		Username: request.Username, Password: request.Password, Role: request.Role, Disabled: request.Disabled,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	auth, _ := authFromRequest(r)
	id := r.PathValue("id")
	if id == auth.User.ID {
		writeErr(w, http.StatusBadRequest, "不能删除当前登录用户")
		return
	}
	if err := s.Store.DeleteUser(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Store.Settings())
}

func (s *Server) handleUpdateAdminSettings(w http.ResponseWriter, r *http.Request) {
	var next config.Config
	if err := decodeJSON(w, r, &next); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.UpdateSettings(next); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, next)
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request, runConfig pipeline.Config) (string, bool) {
	if s.Store == nil {
		return "", true
	}
	auth, ok := authFromRequest(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "请先登录")
		return "", false
	}
	engines := make([]string, 0, len(runConfig.Engines))
	for _, engine := range runConfig.Engines {
		engines = append(engines, engine.Ref)
	}
	arbiterRef := ""
	if runConfig.Arbiter != nil {
		arbiterRef = runConfig.Arbiter.Ref
	}
	task, err := s.Store.CreateTask(auth.User, requestFilename(r), engines, arbiterRef)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建任务记录失败: "+err.Error())
		return "", false
	}
	return task.ID, true
}

func (s *Server) finishTask(id string, final *arbiter.Final, errorMessage string, duration time.Duration) {
	if s.Store == nil || id == "" {
		return
	}
	_ = s.Store.FinishTask(id, final, strings.TrimSpace(errorMessage), duration)
}
