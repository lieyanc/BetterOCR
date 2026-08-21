package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lieyanc/BetterOCR/internal/database"
)

const sessionCookieName = "betterocr_session"

type authContextKey struct{}

type authState struct {
	User database.User
	CSRF string
}

type sessionResponse struct {
	User      database.User `json:"user"`
	CSRFToken string        `json:"csrf_token"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "用户数据库未启用")
		return
	}
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	user, token, csrf, err := s.Store.Login(request.Username, request.Password)
	if err != nil {
		if errors.Is(err, database.ErrNotInitialized) {
			writeErr(w, http.StatusConflict, "请先完成系统初始化")
			return
		}
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	s.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, sessionResponse{User: user, CSRFToken: csrf})
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, _ *http.Request) {
	initialized := true
	if s.Store != nil {
		initialized = s.Store.Initialized()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"initialized": initialized})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "用户数据库未启用")
		return
	}
	if s.Store.Initialized() {
		writeErr(w, http.StatusConflict, "系统已经初始化,请直接登录")
		return
	}
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.Store.InitializeAdmin(request.Username, request.Password); err != nil {
		if errors.Is(err, database.ErrAlreadyInitialized) {
			writeErr(w, http.StatusConflict, "系统已经初始化,请直接登录")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	user, token, csrf, err := s.Store.Login(request.Username, request.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "初始化成功,但自动登录失败: "+err.Error())
		return
	}
	s.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusCreated, sessionResponse{User: user, CSRFToken: csrf})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 7 * 24 * 60 * 60,
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	auth, ok := authFromRequest(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "请先登录")
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{User: auth.User, CSRFToken: auth.CSRF})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && s.Store != nil {
		_ = s.Store.Logout(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Store == nil {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "请先登录")
			return
		}
		user, csrf, ok := s.Store.Authenticate(cookie.Value)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "登录已失效,请重新登录")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			provided := r.Header.Get("X-CSRF-Token")
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(csrf)) != 1 {
				writeErr(w, http.StatusForbidden, "请求校验失败,请刷新页面后重试")
				return
			}
		}
		ctx := context.WithValue(r.Context(), authContextKey{}, authState{User: user, CSRF: csrf})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Store == nil {
			next.ServeHTTP(w, r)
			return
		}
		auth, ok := authFromRequest(r)
		if !ok || auth.User.Role != database.RoleAdmin {
			writeErr(w, http.StatusForbidden, "需要管理员权限")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func authFromRequest(r *http.Request) (authState, bool) {
	auth, ok := r.Context().Value(authContextKey{}).(authState)
	return auth, ok
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("请求 JSON 格式无效: " + err.Error())
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("请求只能包含一个 JSON 值")
	}
	return nil
}

func requestFilename(r *http.Request) string {
	if r.MultipartForm == nil {
		return ""
	}
	files := r.MultipartForm.File["image"]
	if len(files) == 0 {
		return ""
	}
	return strings.TrimSpace(files[0].Filename)
}
