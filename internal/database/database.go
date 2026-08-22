// Package database provides BetterOCR's small, concurrency-safe JSON database.
package database

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
)

const (
	DefaultPath     = "data/database.json"
	databaseVersion = 3
	passwordRounds  = 310_000
	sessionLifetime = 7 * 24 * time.Hour
)

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

func (r Role) Valid() bool { return r == RoleAdmin || r == RoleUser }

// User is the public user representation. Password material is never exposed.
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      Role      `json:"role"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type storedUser struct {
	User
	PasswordSalt string `json:"password_salt"`
	PasswordHash string `json:"password_hash"`
}

type session struct {
	TokenHash string    `json:"token_hash"`
	CSRFToken string    `json:"csrf_token"`
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Task is one persisted OCR run. Images are deliberately not retained.
type Task struct {
	ID               string         `json:"id"`
	UserID           string         `json:"user_id"`
	Username         string         `json:"username"`
	Filename         string         `json:"filename"`
	Status           string         `json:"status"`
	Engines          []string       `json:"engines"`
	Arbiter          string         `json:"arbiter,omitempty"`
	DuplicateChecker string         `json:"duplicate_checker,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	CompletedAt      *time.Time     `json:"completed_at,omitempty"`
	DurationMS       int64          `json:"duration_ms,omitempty"`
	Result           *arbiter.Final `json:"result,omitempty"`
	Error            string         `json:"error,omitempty"`
}

const (
	DocumentPreparing  = "preparing"
	DocumentReady      = "ready"
	DocumentProcessing = "processing"
	DocumentCompleted  = "completed"
	DocumentFailed     = "failed"
	DocumentCancelled  = "cancelled"

	PagePreparing  = "preparing"
	PageReady      = "ready"
	PageQueued     = "queued"
	PageProcessing = "processing"
	PageCompleted  = "completed"
	PageFailed     = "failed"
)

// DocumentProject is lightweight project metadata. Source files, rendered
// page images and full OCR results are stored separately on disk.
type DocumentProject struct {
	ID               string         `json:"id"`
	UserID           string         `json:"user_id"`
	Username         string         `json:"username"`
	Name             string         `json:"name"`
	SourceType       string         `json:"source_type"`
	MimeType         string         `json:"mime_type"`
	SizeBytes        int64          `json:"size_bytes"`
	Status           string         `json:"status"`
	PageCount        int            `json:"page_count"`
	PreparedPages    int            `json:"prepared_pages"`
	ProcessedPages   int            `json:"processed_pages"`
	FailedPages      int            `json:"failed_pages"`
	PendingDisputes  int            `json:"pending_disputes"`
	Engines          []string       `json:"engines"`
	Arbiter          string         `json:"arbiter,omitempty"`
	DuplicateChecker string         `json:"duplicate_checker,omitempty"`
	AutoArbitrate    bool           `json:"auto_arbitrate"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	CompletedAt      *time.Time     `json:"completed_at,omitempty"`
	Error            string         `json:"error,omitempty"`
	Pages            []DocumentPage `json:"pages,omitempty"`
}

type DocumentPage struct {
	ID              string    `json:"id"`
	SourcePage      int       `json:"source_page"`
	PageNumber      int       `json:"page_number"`
	Status          string    `json:"status"`
	ImageReady      bool      `json:"image_ready"`
	ResultReady     bool      `json:"result_ready"`
	Confidence      float64   `json:"confidence,omitempty"`
	Segments        int       `json:"segments,omitempty"`
	PendingDisputes int       `json:"pending_disputes,omitempty"`
	DurationMS      int64     `json:"duration_ms,omitempty"`
	Revision        int       `json:"revision"`
	Error           string    `json:"error,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type data struct {
	Version     int               `json:"version"`
	Initialized bool              `json:"initialized"`
	Users       []storedUser      `json:"users"`
	Sessions    []session         `json:"sessions"`
	Tasks       []Task            `json:"tasks"`
	Documents   []DocumentProject `json:"documents"`
}

type Store struct {
	mu             sync.RWMutex
	path           string
	data           data
	legacySettings json.RawMessage
}

var (
	ErrNotInitialized     = errors.New("系统尚未初始化")
	ErrAlreadyInitialized = errors.New("系统已经初始化")
)

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("JSON 数据库路径不能为空")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && len(strings.TrimSpace(string(raw))) == 0 {
		store := &Store{path: path, data: data{
			Version: databaseVersion, Initialized: false, Users: []storedUser{}, Sessions: []session{}, Tasks: []Task{}, Documents: []DocumentProject{},
		}}
		if err := store.saveLocked(); err != nil {
			return nil, fmt.Errorf("创建 JSON 数据库失败: %w", err)
		}
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var loaded data
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("解析 JSON 数据库 %s 失败: %w", path, err)
	}
	if loaded.Version < 1 || loaded.Version > databaseVersion {
		return nil, fmt.Errorf("JSON 数据库版本 %d 不受支持", loaded.Version)
	}
	needsRewrite := loaded.Version != databaseVersion
	if loaded.Version < 3 {
		// Before v3, prepared but not scheduled pages were also called queued.
		// Preserve actual interrupted queues while making idle documents manageable.
		for documentIndex := range loaded.Documents {
			document := &loaded.Documents[documentIndex]
			if document.Status == DocumentProcessing {
				continue
			}
			for pageIndex := range document.Pages {
				if document.Pages[pageIndex].Status == PageQueued {
					document.Pages[pageIndex].Status = PageReady
				}
			}
		}
	}
	loaded.Version = databaseVersion
	if !loaded.Initialized && len(loaded.Users) > 0 {
		// Version 1 databases created before web setup did not carry this flag.
		loaded.Initialized = true
		needsRewrite = true
	}
	if loaded.Initialized {
		activeAdmins := 0
		for _, user := range loaded.Users {
			if user.Role == RoleAdmin && !user.Disabled {
				activeAdmins++
			}
		}
		if activeAdmins == 0 {
			return nil, errors.New("JSON 数据库没有可用的管理员用户")
		}
	} else if len(loaded.Users) != 0 {
		return nil, errors.New("未初始化的 JSON 数据库不能包含用户")
	}
	var legacy struct {
		Settings json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, fmt.Errorf("解析 JSON 数据库 %s 失败: %w", path, err)
	}
	store := &Store{path: path, data: loaded, legacySettings: append(json.RawMessage(nil), legacy.Settings...)}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	// Legacy Web settings must first be migrated back to config.json by the
	// caller. Until then, keep the old file intact so no configuration is lost.
	if needsRewrite && len(store.legacySettings) == 0 {
		if err := store.saveLocked(); err != nil {
			return nil, fmt.Errorf("迁移 JSON 数据库失败: %w", err)
		}
	}
	return store, nil
}

// LegacySettings returns the settings embedded by database version 1. New
// databases never contain this field.
func (s *Store) LegacySettings() json.RawMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append(json.RawMessage(nil), s.legacySettings...)
}

// DiscardLegacySettings rewrites the database without the obsolete settings
// field after its value has been safely moved to the configuration file.
func (s *Store) DiscardLegacySettings() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.legacySettings) == 0 {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) Initialized() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Initialized
}

func (s *Store) InitializeAdmin(username, password string) (User, error) {
	s.mu.RLock()
	initialized := s.data.Initialized
	s.mu.RUnlock()
	if initialized {
		return User{}, ErrAlreadyInitialized
	}
	admin, err := newStoredUser(username, password, RoleAdmin)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Initialized {
		return User{}, ErrAlreadyInitialized
	}
	previous := s.data
	s.data.Initialized = true
	s.data.Users = []storedUser{admin}
	s.data.Sessions = []session{}
	if err := s.saveLocked(); err != nil {
		s.data = previous
		return User{}, err
	}
	return admin.User, nil
}

func (s *Store) Login(username, password string) (User, string, string, error) {
	canonical := canonicalUsername(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.data.Initialized {
		return User{}, "", "", ErrNotInitialized
	}
	var matched *storedUser
	for i := range s.data.Users {
		if canonicalUsername(s.data.Users[i].Username) == canonical {
			matched = &s.data.Users[i]
			break
		}
	}
	if matched == nil || matched.Disabled || !checkPassword(*matched, password) {
		return User{}, "", "", errors.New("用户名或密码错误")
	}
	token, err := randomToken(32)
	if err != nil {
		return User{}, "", "", err
	}
	csrf, err := randomToken(24)
	if err != nil {
		return User{}, "", "", err
	}
	now := time.Now().UTC()
	previousSessions := append([]session(nil), s.data.Sessions...)
	active := s.data.Sessions[:0]
	for _, current := range s.data.Sessions {
		if current.ExpiresAt.After(now) {
			active = append(active, current)
		}
	}
	s.data.Sessions = append(active, session{
		TokenHash: tokenHash(token), CSRFToken: csrf, UserID: matched.ID, ExpiresAt: now.Add(sessionLifetime),
	})
	if err := s.saveLocked(); err != nil {
		s.data.Sessions = previousSessions
		return User{}, "", "", err
	}
	return matched.User, token, csrf, nil
}

func (s *Store) Authenticate(token string) (User, string, bool) {
	if token == "" {
		return User{}, "", false
	}
	hash := tokenHash(token)
	now := time.Now().UTC()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, current := range s.data.Sessions {
		if current.TokenHash != hash || !current.ExpiresAt.After(now) {
			continue
		}
		for _, user := range s.data.Users {
			if user.ID == current.UserID && !user.Disabled {
				return user.User, current.CSRFToken, true
			}
		}
	}
	return User{}, "", false
}

func (s *Store) Logout(token string) error {
	hash := tokenHash(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	previousSessions := append([]session(nil), s.data.Sessions...)
	before := len(previousSessions)
	s.data.Sessions = slices.DeleteFunc(s.data.Sessions, func(current session) bool {
		return current.TokenHash == hash
	})
	if len(s.data.Sessions) == before {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		s.data.Sessions = previousSessions
		return err
	}
	return nil
}

func (s *Store) Users() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, len(s.data.Users))
	for i, user := range s.data.Users {
		out[i] = user.User
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(a.Username, b.Username) })
	return out
}

func (s *Store) CreateUser(username, password string, role Role) (User, error) {
	created, err := newStoredUser(username, password, role)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.Users {
		if canonicalUsername(existing.Username) == canonicalUsername(created.Username) {
			return User{}, errors.New("用户名已存在")
		}
	}
	s.data.Users = append(s.data.Users, created)
	if err := s.saveLocked(); err != nil {
		s.data.Users = s.data.Users[:len(s.data.Users)-1]
		return User{}, err
	}
	return created.User, nil
}

type UserUpdate struct {
	Username string
	Password string
	Role     *Role
	Disabled *bool
}

func (s *Store) UpdateUser(id string, update UserUpdate) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := slices.IndexFunc(s.data.Users, func(user storedUser) bool { return user.ID == id })
	if index < 0 {
		return User{}, errors.New("用户不存在")
	}
	previous := s.data.Users[index]
	previousSessions := append([]session(nil), s.data.Sessions...)
	next := previous
	if strings.TrimSpace(update.Username) != "" {
		if err := validateUsername(update.Username); err != nil {
			return User{}, err
		}
		next.Username = strings.TrimSpace(update.Username)
		for i, existing := range s.data.Users {
			if i != index && canonicalUsername(existing.Username) == canonicalUsername(next.Username) {
				return User{}, errors.New("用户名已存在")
			}
		}
	}
	if update.Role != nil {
		if !update.Role.Valid() {
			return User{}, errors.New("用户角色无效")
		}
		next.Role = *update.Role
	}
	if update.Disabled != nil {
		next.Disabled = *update.Disabled
	}
	if update.Password != "" {
		if err := setPassword(&next, update.Password); err != nil {
			return User{}, err
		}
	}
	if previous.Role == RoleAdmin && !previous.Disabled && (next.Role != RoleAdmin || next.Disabled) && s.activeAdminsLocked() == 1 {
		return User{}, errors.New("不能停用或降级最后一个管理员")
	}
	next.UpdatedAt = time.Now().UTC()
	s.data.Users[index] = next
	if previous.Role != next.Role || previous.Disabled != next.Disabled || update.Password != "" {
		s.data.Sessions = slices.DeleteFunc(s.data.Sessions, func(current session) bool {
			return current.UserID == id
		})
	}
	if err := s.saveLocked(); err != nil {
		s.data.Users[index] = previous
		s.data.Sessions = previousSessions
		return User{}, err
	}
	return next.User, nil
}

func (s *Store) DeleteUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := slices.IndexFunc(s.data.Users, func(user storedUser) bool { return user.ID == id })
	if index < 0 {
		return errors.New("用户不存在")
	}
	user := s.data.Users[index]
	if user.Role == RoleAdmin && !user.Disabled && s.activeAdminsLocked() == 1 {
		return errors.New("不能删除最后一个管理员")
	}
	previousUsers := append([]storedUser(nil), s.data.Users...)
	previousSessions := append([]session(nil), s.data.Sessions...)
	s.data.Users = slices.Delete(s.data.Users, index, index+1)
	s.data.Sessions = slices.DeleteFunc(s.data.Sessions, func(current session) bool { return current.UserID == id })
	if err := s.saveLocked(); err != nil {
		s.data.Users = previousUsers
		s.data.Sessions = previousSessions
		return err
	}
	return nil
}

func (s *Store) CreateTask(user User, filename string, engines []string, arbiterRef, duplicateCheckerRef string) (Task, error) {
	id, err := randomToken(12)
	if err != nil {
		return Task{}, err
	}
	task := Task{
		ID: id, UserID: user.ID, Username: user.Username, Filename: strings.TrimSpace(filename),
		Status: "running", Engines: append([]string(nil), engines...), Arbiter: arbiterRef,
		DuplicateChecker: strings.TrimSpace(duplicateCheckerRef), CreatedAt: time.Now().UTC(),
	}
	if task.Filename == "" {
		task.Filename = "未命名图片"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Tasks = append(s.data.Tasks, task)
	if err := s.saveLocked(); err != nil {
		s.data.Tasks = s.data.Tasks[:len(s.data.Tasks)-1]
		return Task{}, err
	}
	return task, nil
}

func (s *Store) FinishTask(id string, result *arbiter.Final, errorMessage string, duration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := slices.IndexFunc(s.data.Tasks, func(task Task) bool { return task.ID == id })
	if index < 0 {
		return errors.New("任务不存在")
	}
	previous := s.data.Tasks[index]
	completed := time.Now().UTC()
	s.data.Tasks[index].CompletedAt = &completed
	s.data.Tasks[index].DurationMS = duration.Milliseconds()
	s.data.Tasks[index].Error = errorMessage
	if result != nil {
		copyResult := *result
		s.data.Tasks[index].Result = &copyResult
		s.data.Tasks[index].Status = "completed"
	} else {
		s.data.Tasks[index].Status = "failed"
	}
	if err := s.saveLocked(); err != nil {
		s.data.Tasks[index] = previous
		return err
	}
	return nil
}

func (s *Store) Tasks(userID string, all bool) []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Task, 0, len(s.data.Tasks))
	for i := len(s.data.Tasks) - 1; i >= 0; i-- {
		if all || s.data.Tasks[i].UserID == userID {
			out = append(out, s.data.Tasks[i])
		}
	}
	if out == nil {
		return []Task{}
	}
	return out
}

// Directory returns the database directory. Document payloads live beside the
// JSON database so a custom -db path remains self-contained.
func (s *Store) Directory() string {
	return filepath.Dir(s.path)
}

func (s *Store) CreateDocument(
	user User,
	name, sourceType, mimeType string,
	sizeBytes int64,
	engines []string,
	arbiterRef string,
	duplicateCheckerRef string,
	autoArbitrate bool,
) (DocumentProject, error) {
	id, err := randomToken(12)
	if err != nil {
		return DocumentProject{}, err
	}
	now := time.Now().UTC()
	document := DocumentProject{
		ID: id, UserID: user.ID, Username: user.Username,
		Name: strings.TrimSpace(name), SourceType: sourceType, MimeType: mimeType,
		SizeBytes: sizeBytes, Status: DocumentPreparing,
		Engines: append([]string(nil), engines...), Arbiter: strings.TrimSpace(arbiterRef),
		DuplicateChecker: strings.TrimSpace(duplicateCheckerRef),
		AutoArbitrate:    autoArbitrate, CreatedAt: now, UpdatedAt: now,
		Pages: []DocumentPage{},
	}
	if document.Name == "" {
		document.Name = "未命名文档"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Documents = append(s.data.Documents, document)
	if err := s.saveLocked(); err != nil {
		s.data.Documents = s.data.Documents[:len(s.data.Documents)-1]
		return DocumentProject{}, err
	}
	return cloneDocument(document), nil
}

func (s *Store) Document(id string) (DocumentProject, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	index := slices.IndexFunc(s.data.Documents, func(document DocumentProject) bool { return document.ID == id })
	if index < 0 {
		return DocumentProject{}, false
	}
	return cloneDocument(s.data.Documents[index]), true
}

func (s *Store) Documents(userID string, all bool) []DocumentProject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DocumentProject, 0, len(s.data.Documents))
	for _, document := range s.data.Documents {
		if all || document.UserID == userID {
			out = append(out, cloneDocument(document))
		}
	}
	slices.SortFunc(out, func(a, b DocumentProject) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	if out == nil {
		return []DocumentProject{}
	}
	return out
}

// MutateDocument applies one atomic metadata update and persists it before
// returning. The callback receives a private copy and cannot escape the lock.
func (s *Store) MutateDocument(id string, mutate func(*DocumentProject) error) (DocumentProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := slices.IndexFunc(s.data.Documents, func(document DocumentProject) bool { return document.ID == id })
	if index < 0 {
		return DocumentProject{}, errors.New("文档项目不存在")
	}
	previous := cloneDocument(s.data.Documents[index])
	next := cloneDocument(previous)
	if err := mutate(&next); err != nil {
		return DocumentProject{}, err
	}
	recountDocument(&next)
	next.UpdatedAt = time.Now().UTC()
	s.data.Documents[index] = next
	if err := s.saveLocked(); err != nil {
		s.data.Documents[index] = previous
		return DocumentProject{}, err
	}
	return cloneDocument(next), nil
}

func (s *Store) DeleteDocument(id string) (DocumentProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := slices.IndexFunc(s.data.Documents, func(document DocumentProject) bool { return document.ID == id })
	if index < 0 {
		return DocumentProject{}, errors.New("文档项目不存在")
	}
	deleted := cloneDocument(s.data.Documents[index])
	previous := append([]DocumentProject(nil), s.data.Documents...)
	s.data.Documents = slices.Delete(s.data.Documents, index, index+1)
	if err := s.saveLocked(); err != nil {
		s.data.Documents = previous
		return DocumentProject{}, err
	}
	return deleted, nil
}

type DocumentRecovery struct {
	Prepare []string
	Process []string
}

// FailInterruptedTasks closes out single-image tasks that were still running
// when the process stopped. 它们绑定在请求生命周期上,重启后没人再收尾,
// 不处理就会永远停在 running。返回被收尾的任务数。
func (s *Store) FailInterruptedTasks() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]Task(nil), s.data.Tasks...)
	completed := time.Now().UTC()
	changed := 0
	for i := range s.data.Tasks {
		if s.data.Tasks[i].Status != "running" {
			continue
		}
		s.data.Tasks[i].Status = "failed"
		s.data.Tasks[i].CompletedAt = &completed
		s.data.Tasks[i].Error = "服务重启中断了这次识别"
		changed++
	}
	if changed == 0 {
		return 0, nil
	}
	if err := s.saveLocked(); err != nil {
		s.data.Tasks = previous
		return 0, err
	}
	return changed, nil
}

// RecoverDocuments converts interrupted page operations into resumable states
// and returns the jobs that should be placed back on the background queue.
func (s *Store) RecoverDocuments() (DocumentRecovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := make([]DocumentProject, len(s.data.Documents))
	for i := range s.data.Documents {
		previous[i] = cloneDocument(s.data.Documents[i])
	}
	var recovery DocumentRecovery
	changed := false
	for i := range s.data.Documents {
		document := &s.data.Documents[i]
		switch document.Status {
		case DocumentPreparing:
			recovery.Prepare = append(recovery.Prepare, document.ID)
			for pageIndex := range document.Pages {
				if document.Pages[pageIndex].ImageReady {
					document.Pages[pageIndex].Status = PageReady
				} else {
					document.Pages[pageIndex].Status = PagePreparing
				}
			}
			changed = true
		case DocumentProcessing:
			recovery.Process = append(recovery.Process, document.ID)
			for pageIndex := range document.Pages {
				if document.Pages[pageIndex].Status == PageProcessing {
					document.Pages[pageIndex].Status = PageQueued
				}
			}
			document.Status = DocumentProcessing
			changed = true
		}
		recountDocument(document)
	}
	if !changed {
		return recovery, nil
	}
	if err := s.saveLocked(); err != nil {
		s.data.Documents = previous
		return DocumentRecovery{}, err
	}
	return recovery, nil
}

func cloneDocument(source DocumentProject) DocumentProject {
	cloned := source
	cloned.Engines = append([]string(nil), source.Engines...)
	cloned.Pages = append([]DocumentPage(nil), source.Pages...)
	if source.CompletedAt != nil {
		completed := *source.CompletedAt
		cloned.CompletedAt = &completed
	}
	return cloned
}

func recountDocument(document *DocumentProject) {
	document.PageCount = len(document.Pages)
	document.PreparedPages = 0
	document.ProcessedPages = 0
	document.FailedPages = 0
	document.PendingDisputes = 0
	for pageIndex := range document.Pages {
		page := &document.Pages[pageIndex]
		page.PageNumber = pageIndex + 1
		if page.ImageReady {
			document.PreparedPages++
		}
		if page.Status == PageCompleted {
			document.ProcessedPages++
		}
		if page.Status == PageFailed {
			document.FailedPages++
		}
		document.PendingDisputes += page.PendingDisputes
	}
}

func (s *Store) activeAdminsLocked() int {
	count := 0
	for _, user := range s.data.Users {
		if user.Role == RoleAdmin && !user.Disabled {
			count++
		}
	}
	return count
}

func newStoredUser(username, password string, role Role) (storedUser, error) {
	if err := validateUsername(username); err != nil {
		return storedUser{}, err
	}
	if !role.Valid() {
		return storedUser{}, errors.New("用户角色无效")
	}
	id, err := randomToken(12)
	if err != nil {
		return storedUser{}, err
	}
	now := time.Now().UTC()
	user := storedUser{User: User{
		ID: id, Username: strings.TrimSpace(username), Role: role, CreatedAt: now, UpdatedAt: now,
	}}
	if err := setPassword(&user, password); err != nil {
		return storedUser{}, err
	}
	return user, nil
}

func validateUsername(username string) error {
	username = strings.TrimSpace(username)
	length := utf8.RuneCountInString(username)
	if length < 2 || length > 32 {
		return errors.New("用户名长度必须为 2 到 32 个字符")
	}
	for _, r := range username {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("/\\:'\"", r) {
			return errors.New("用户名不能包含空白、控制字符或 / \\ : ' \"")
		}
	}
	return nil
}

func setPassword(user *storedUser, password string) error {
	if len(password) < 8 {
		return errors.New("密码至少需要 8 个字符")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	derived, err := pbkdf2.Key(sha256.New, password, salt, passwordRounds, 32)
	if err != nil {
		return err
	}
	user.PasswordSalt = base64.RawStdEncoding.EncodeToString(salt)
	user.PasswordHash = base64.RawStdEncoding.EncodeToString(derived)
	return nil
}

func checkPassword(user storedUser, password string) bool {
	salt, err := base64.RawStdEncoding.DecodeString(user.PasswordSalt)
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(user.PasswordHash)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, passwordRounds, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func canonicalUsername(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

func randomToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Store) saveLocked() error {
	out, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".betterocr-db-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	s.legacySettings = nil
	return os.Chmod(s.path, 0o600)
}
