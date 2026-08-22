// Package updater implements BetterOCR's in-place OTA self-update: it discovers
// releases on GitHub (directly or through a mirror), downloads the binary for
// the running platform, verifies its SHA256 against the checksum published by
// CI, and replaces the running process without changing its PID on Unix.
//
// 更新只在 Web 模式下启用,且默认关闭(config.json 的 update.enabled)。
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lieyanc/BetterOCR/internal/version"
)

// Config is the update block of data/config.json. Authorization is not part of
// it: the HTTP endpoints reuse BetterOCR's admin session guard.
type Config struct {
	Enabled       bool   `json:"enabled"`
	Channel       string `json:"channel"`
	CheckInterval int    `json:"check_interval"`
	Source        string `json:"source"`
	ProxyBaseURL  string `json:"proxy_base_url"`
	Repo          string `json:"repo"`
}

// Status is the state-machine snapshot polled by the admin UI.
type Status struct {
	State            string  `json:"state"`
	CurrentVersion   string  `json:"current_version"`
	LatestVersion    string  `json:"latest_version,omitempty"`
	IsPrerelease     bool    `json:"is_prerelease"`
	Progress         float64 `json:"progress,omitempty"`
	DownloadProgress float64 `json:"download_progress,omitempty"`
	Error            string  `json:"error,omitempty"`
	LastCheck        string  `json:"last_check,omitempty"`
	ReleaseNotes     string  `json:"release_notes,omitempty"`
}

// CheckResult is the answer to an explicit check that does not download.
type CheckResult struct {
	HasUpdate      bool   `json:"has_update"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	IsPrerelease   bool   `json:"is_prerelease"`
	ReleaseNotes   string `json:"release_notes,omitempty"`
	Channel        string `json:"channel"`
}

// RestartHooks lets the host shut down cleanly around the process replacement.
type RestartHooks struct {
	// BeforeExec runs last before the binary is swapped. Returning an error
	// aborts the apply, leaving the current version running.
	BeforeExec func(tag string) error
	// OnExecFailure reports a failed process replacement to the host, which
	// owns the decision to keep serving or exit.
	OnExecFailure func(error)
	// IsBusy reports whether work is in flight that a restart would destroy.
	// When set, the updater waits for it to return false (bounded by
	// idleWaitTimeout) before applying an update.
	IsBusy func() bool
}

type Updater struct {
	cfg     func() Config
	dataDir func() string
	logger  *log.Logger
	hooks   RestartHooks

	mu     sync.RWMutex
	status Status

	bgCtx context.Context

	pendingBinaryPath string
	pendingTag        string
}

const (
	idleWaitTimeout  = 10 * time.Minute
	idlePollInterval = 5 * time.Second
	downloadTimeout  = 30 * time.Minute

	// SourceGitHub downloads release assets straight from
	// github.com/<repo>/releases/download/... links, which are CDN redirects
	// and not subject to GitHub REST API rate limits.
	SourceGitHub = "github"
	// SourceProxy routes release lookups and downloads through the configured
	// proxy_base_url mirror.
	SourceProxy = "proxy"

	// DefaultRepo is the upstream release repository.
	DefaultRepo = "lieyanc/BetterOCR"
	// DefaultProxyBaseURL mirrors GitHub releases for networks that cannot
	// reach github.com directly.
	DefaultProxyBaseURL = "https://dl.repo.chycloud.top"
	// DefaultCheckInterval is the background check period in seconds.
	DefaultCheckInterval = 3600

	progressChecking      = 5
	progressReleaseFound  = 10
	progressDownloadStart = 10
	progressDownloadDone  = 90
	progressVerifyStart   = 92
	progressVerifyDone    = 95
	progressApplying      = 98
	progressComplete      = 100
)

// New wires an updater. cfg and dataDir are functions rather than values so a
// configuration saved through the admin API takes effect without a restart.
func New(cfg func() Config, dataDir func() string, logger *log.Logger, hooks RestartHooks) *Updater {
	if logger == nil {
		logger = log.Default()
	}
	return &Updater{
		cfg:     cfg,
		dataDir: dataDir,
		logger:  logger,
		hooks:   hooks,
		status: Status{
			State:          "idle",
			CurrentVersion: version.Version,
		},
	}
}

func (u *Updater) Status() Status {
	u.mu.RLock()
	defer u.mu.RUnlock()
	s := u.status
	s.CurrentVersion = version.Version
	return s
}

// CheckOnly resolves the latest release without downloading anything.
func (u *Updater) CheckOnly(ctx context.Context) (CheckResult, error) {
	cfg := normalizeConfig(u.cfg())
	result := CheckResult{
		CurrentVersion: version.Version,
		Channel:        cfg.Channel,
	}

	release, hasUpdate, err := u.checkForUpdate(ctx, cfg)
	if err != nil {
		return result, err
	}

	u.mu.Lock()
	u.status.LastCheck = time.Now().UTC().Format(time.RFC3339)
	// 一次成功的检查让上一次失败翻篇,否则界面会把过期错误和新结果一起显示。
	if u.status.State == "failed" {
		u.status.State = "idle"
		u.status.Error = ""
	}
	u.mu.Unlock()

	if release == nil {
		return result, nil
	}

	result.HasUpdate = hasUpdate
	result.LatestVersion = release.displayVersion()
	result.IsPrerelease = release.Prerelease
	result.ReleaseNotes = release.Body

	u.mu.Lock()
	u.status.LatestVersion = release.displayVersion()
	u.status.IsPrerelease = release.Prerelease
	u.status.ReleaseNotes = release.Body
	u.mu.Unlock()

	return result, nil
}

// StartUpdate runs check, download and (on the stable channel) apply in the
// background. The request context is deliberately ignored: it is canceled as
// soon as the HTTP response is written, which would abort the download.
func (u *Updater) StartUpdate(_ context.Context) {
	go u.performUpdate(u.bgContext())
}

// ApplyPending restarts into an already downloaded pre-release build.
func (u *Updater) ApplyPending(_ context.Context) error {
	u.mu.Lock()
	state := u.status.State
	path := u.pendingBinaryPath
	tag := u.pendingTag

	if state != "ready" || path == "" {
		u.mu.Unlock()
		return fmt.Errorf("no pending update to apply")
	}

	// 先同步置位再起 goroutine:重复调用立刻被拒,前端轮询也立刻看到 applying。
	u.status.State = "applying"
	u.status.Progress = progressApplying
	u.status.DownloadProgress = 0
	u.pendingBinaryPath = ""
	u.pendingTag = ""
	u.mu.Unlock()

	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := u.waitForIdle(u.bgContext()); err != nil {
			u.setError("apply canceled while waiting for idle: " + err.Error())
			return
		}
		if err := u.applyUpdate(path, tag); err != nil {
			u.notifyExecFailure(err)
			u.setError("apply failed: " + err.Error())
		}
	}()
	return nil
}

// DismissPending drops a downloaded pre-release build and returns to idle.
func (u *Updater) DismissPending() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.status.State == "ready" {
		if u.pendingBinaryPath != "" {
			_ = os.Remove(u.pendingBinaryPath)
		}
		u.pendingBinaryPath = ""
		u.pendingTag = ""
		u.status.State = "idle"
		u.status.LatestVersion = ""
		u.status.Progress = 0
		u.status.DownloadProgress = 0
		u.status.Error = ""
	}
}

// StartBackground remembers the application context and starts the periodic
// check loop. Manual updates reuse that context, never a request context, so
// it is stored even when periodic checks are disabled.
func (u *Updater) StartBackground(ctx context.Context) {
	cfg := normalizeConfig(u.cfg())
	u.mu.Lock()
	u.bgCtx = ctx
	u.mu.Unlock()
	if !cfg.Enabled {
		u.logger.Printf("update: periodic checks disabled")
		return
	}
	u.logger.Printf("update: enabled, channel=%s, source=%s, repo=%s, interval=%ds",
		cfg.Channel, cfg.Source, cfg.Repo, cfg.CheckInterval)
	go u.loop(ctx)
}

func (u *Updater) bgContext() context.Context {
	u.mu.RLock()
	ctx := u.bgCtx
	u.mu.RUnlock()
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func (u *Updater) loop(ctx context.Context) {
	// 让服务先启动稳定,再做首次检查。
	select {
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return
	}

	u.checkAndUpdate(ctx)

	for {
		cfg := normalizeConfig(u.cfg())
		interval := time.Duration(cfg.CheckInterval) * time.Second
		if interval < time.Minute {
			interval = time.Minute
		}
		select {
		case <-time.After(interval):
			u.checkAndUpdate(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (u *Updater) checkAndUpdate(ctx context.Context) {
	cfg := normalizeConfig(u.cfg())
	if !cfg.Enabled {
		return
	}
	u.performUpdate(ctx)
}

func (u *Updater) performUpdate(ctx context.Context) {
	cfg := normalizeConfig(u.cfg())

	u.mu.Lock()
	if u.status.State == "checking" || u.status.State == "ready" || u.status.State == "downloading" || u.status.State == "applying" {
		u.mu.Unlock()
		return
	}
	u.status.State = "checking"
	u.status.Progress = progressChecking
	u.status.Error = ""
	u.status.DownloadProgress = 0
	u.mu.Unlock()

	release, hasUpdate, err := u.checkForUpdate(ctx, cfg)
	if err != nil {
		u.setError("check failed: " + err.Error())
		return
	}
	if release == nil || !hasUpdate {
		u.mu.Lock()
		u.status.State = "idle"
		u.status.Progress = 0
		u.status.DownloadProgress = 0
		u.status.LastCheck = time.Now().UTC().Format(time.RFC3339)
		u.mu.Unlock()
		return
	}

	u.mu.Lock()
	u.status.LatestVersion = release.displayVersion()
	u.status.IsPrerelease = release.Prerelease
	u.status.ReleaseNotes = release.Body
	u.status.LastCheck = time.Now().UTC().Format(time.RFC3339)
	u.status.Progress = progressReleaseFound
	u.mu.Unlock()

	binaryPath, err := u.download(ctx, cfg, release)
	if err != nil {
		u.setError("download failed: " + err.Error())
		return
	}

	// stable 语义是"用户已选择自动更新到正式版",所以下载完直接装;
	// dev 是滚动预发布,停在 ready 等管理员确认。
	if cfg.Channel == "stable" {
		u.mu.Lock()
		u.status.State = "applying"
		u.status.Progress = progressApplying
		u.status.DownloadProgress = 0
		u.mu.Unlock()
		if err := u.waitForIdle(ctx); err != nil {
			u.setError("apply canceled while waiting for idle: " + err.Error())
			return
		}
		if err := u.applyUpdate(binaryPath, release.TagName); err != nil {
			u.notifyExecFailure(err)
			u.setError("apply failed: " + err.Error())
		}
		return
	}

	u.mu.Lock()
	u.status.State = "ready"
	u.status.Progress = progressVerifyDone
	u.status.DownloadProgress = 0
	u.pendingBinaryPath = binaryPath
	u.pendingTag = release.TagName
	u.mu.Unlock()
	u.logger.Printf("update: pre-release %s ready, waiting for admin confirmation", release.TagName)
}

func (u *Updater) setError(msg string) {
	u.logger.Printf("update: %s", msg)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.State = "failed"
	u.status.Error = msg
	u.status.LastCheck = time.Now().UTC().Format(time.RFC3339)
}

func clampProgress(progress float64) float64 {
	if progress < 0 {
		return 0
	}
	if progress > 100 {
		return 100
	}
	return progress
}

func overallDownloadProgress(downloadProgress float64) float64 {
	downloadProgress = clampProgress(downloadProgress)
	span := progressDownloadDone - progressDownloadStart
	return progressDownloadStart + downloadProgress*float64(span)/100
}

func (u *Updater) notifyExecFailure(err error) {
	if err == nil || u.hooks.OnExecFailure == nil {
		return
	}
	u.hooks.OnExecFailure(err)
}

// waitForIdle blocks until the application reports no in-flight work. A
// canceled application context aborts the update; only the deliberate idle
// timeout permits applying while work is still reported as active.
func (u *Updater) waitForIdle(ctx context.Context) error {
	if u.hooks.IsBusy == nil || !u.hooks.IsBusy() {
		return nil
	}
	u.logger.Printf("update: waiting for in-flight work to finish before applying (max %s)", idleWaitTimeout)
	deadline := time.After(idleWaitTimeout)
	ticker := time.NewTicker(idlePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			u.logger.Printf("update: idle wait timed out, applying anyway")
			return nil
		case <-ticker.C:
			if !u.hooks.IsBusy() {
				return nil
			}
		}
	}
}

type releaseInfo struct {
	TagName         string      `json:"tag_name"`
	TargetCommitish string      `json:"target_commitish"`
	Prerelease      bool        `json:"prerelease"`
	Body            string      `json:"body"`
	Assets          []assetInfo `json:"assets"`
	Version         string
	Commit          string
	BuildTime       string
}

type assetInfo struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type releaseVersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Tag       string `json:"tag"`
}

func (r releaseInfo) displayVersion() string {
	if strings.TrimSpace(r.Version) != "" {
		return strings.TrimSpace(r.Version)
	}
	return r.TagName
}

// githubBaseURL is a var so tests can point direct-source checks at a local
// server.
var githubBaseURL = "https://github.com"

func (u *Updater) checkForUpdate(ctx context.Context, cfg Config) (*releaseInfo, bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var (
		release *releaseInfo
		err     error
	)
	if cfg.Source == SourceProxy {
		release, err = u.fetchReleaseViaProxy(checkCtx, cfg)
	} else {
		release, err = u.fetchReleaseFromGitHub(checkCtx, cfg)
	}
	if err != nil || release == nil {
		return nil, false, err
	}
	if !u.isNewer(*release, cfg.Channel) {
		u.logger.Printf("update: already up to date (%s)", release.displayVersion())
		return release, false, nil
	}
	return release, true, nil
}

// fetchReleaseFromGitHub resolves the latest release without touching the
// GitHub REST API: it fetches version.json straight from the release download
// URL (fixed "dev" tag, or the "latest" redirect for stable) and synthesizes
// the asset list from the tag it names. No REST call means no rate limit, at
// the cost of release notes, which only the API carries.
func (u *Updater) fetchReleaseFromGitHub(ctx context.Context, cfg Config) (*releaseInfo, error) {
	base := githubBaseURL + "/" + cfg.Repo + "/releases"
	versionURL := base + "/latest/download/version.json"
	if cfg.Channel != "stable" {
		versionURL = base + "/download/dev/version.json"
	}
	u.logger.Printf("update: checking %s", versionURL)

	body, status, err := u.httpGet(ctx, versionURL, 16*1024)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		u.logger.Printf("update: no release found for channel %s", cfg.Channel)
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", status)
	}

	var info releaseVersionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode version metadata: %w", err)
	}
	tag := strings.TrimSpace(info.Tag)
	if cfg.Channel != "stable" {
		tag = "dev"
	} else if tag == "" {
		return nil, fmt.Errorf("version metadata missing release tag")
	}

	targetName := u.targetName()
	assetNames := []string{targetName, targetName + ".sha256", "version.json"}
	assets := make([]assetInfo, 0, len(assetNames))
	for _, name := range assetNames {
		assets = append(assets, assetInfo{
			Name:               name,
			BrowserDownloadURL: base + "/download/" + tag + "/" + name,
		})
	}
	return &releaseInfo{
		TagName:    tag,
		Prerelease: cfg.Channel != "stable",
		Assets:     assets,
		Version:    strings.TrimSpace(info.Version),
		Commit:     strings.TrimSpace(info.Commit),
		BuildTime:  strings.TrimSpace(info.BuildTime),
	}, nil
}

func (u *Updater) fetchReleaseViaProxy(ctx context.Context, cfg Config) (*releaseInfo, error) {
	tag := "latest"
	if cfg.Channel != "stable" {
		tag = "dev"
	}

	url := fmt.Sprintf("%s/api/releases/%s/%s", strings.TrimRight(cfg.ProxyBaseURL, "/"), cfg.Repo, tag)
	u.logger.Printf("update: checking %s", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		u.logger.Printf("update: no release found for channel %s", cfg.Channel)
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var release releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if cfg.Channel != "stable" {
		// 仅补展示用元数据,失败不致命。
		if err := u.loadReleaseVersion(ctx, cfg, &release); err != nil {
			u.logger.Printf("update: version metadata unavailable for %s: %v", release.TagName, err)
		}
	}
	return &release, nil
}

func (u *Updater) loadReleaseVersion(ctx context.Context, cfg Config, release *releaseInfo) error {
	var versionAsset *assetInfo
	for i := range release.Assets {
		if release.Assets[i].Name == "version.json" {
			versionAsset = &release.Assets[i]
			break
		}
	}
	if versionAsset == nil {
		return fmt.Errorf("version.json asset not found")
	}

	body, status, err := u.httpGet(ctx, u.resolveDownloadURL(cfg, versionAsset.BrowserDownloadURL), 16*1024)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("version metadata returned status %d", status)
	}
	var info releaseVersionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("decode version metadata: %w", err)
	}
	release.Version = strings.TrimSpace(info.Version)
	release.Commit = strings.TrimSpace(info.Commit)
	release.BuildTime = strings.TrimSpace(info.BuildTime)
	return nil
}

func (u *Updater) isNewer(release releaseInfo, channel string) bool {
	current := version.Version
	if current == "dev" {
		// 本地裸编译没有可比版本,永远视为可更新。
		return true
	}
	remoteTag := release.TagName
	if channel == "stable" {
		return semverGreater(remoteTag, current)
	}

	// dev 通道用 commit 相等性而非大小:force-push 或 rerun 后 run 号
	// 不可靠,commit 不同就该跟进。
	remoteCommit := normalizeCommit(release.Commit)
	if remoteCommit == "" {
		remoteCommit = normalizeCommit(release.TargetCommitish)
	}
	currentCommit := normalizeCommit(version.Commit)
	if remoteCommit != "" && currentCommit != "" {
		return remoteCommit != currentCommit
	}

	remoteVersion := release.displayVersion()
	if remoteTag == "dev" && remoteVersion == "dev" {
		u.logger.Printf("update: dev release missing comparable commit current=%s remote=%s, skipping", current, remoteTag)
		return false
	}

	remoteNum, remoteSHA := parseDevTag(remoteVersion)
	localNum, localSHA := parseDevTag(current)
	if remoteSHA != "" && localSHA != "" && remoteSHA == localSHA {
		return false
	}
	if remoteNum > 0 && localNum > 0 {
		return remoteNum > localNum
	}
	// 比不出来时宁可不更新,也不能陷入自更新循环。
	u.logger.Printf("update: cannot compare versions current=%s remote=%s, skipping", current, remoteTag)
	return false
}

func normalizeCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if commit == "" || commit == "unknown" {
		return ""
	}
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

func semverGreater(a, b string) bool {
	av := parseSemver(strings.TrimPrefix(a, "v"))
	bv := parseSemver(strings.TrimPrefix(b, "v"))
	for i := 0; i < 3; i++ {
		if av[i] > bv[i] {
			return true
		}
		if av[i] < bv[i] {
			return false
		}
	}
	return false
}

func parseSemver(s string) [3]int {
	var result [3]int
	parts := strings.SplitN(s, ".", 3)
	for i, p := range parts {
		if i >= 3 {
			break
		}
		if idx := strings.IndexByte(p, '-'); idx >= 0 {
			p = p[:idx]
		}
		n, _ := strconv.Atoi(p)
		result[i] = n
	}
	return result
}

// parseDevTag splits dev-{run}-{yyyymmdd}-{shortsha}. 该格式是 CI 与客户端
// 共同依赖的协议,改 workflow 必须同步改这里。
func parseDevTag(tag string) (runNumber int, sha string) {
	parts := strings.SplitN(tag, "-", 4)
	if len(parts) >= 4 && parts[0] == "dev" {
		n, _ := strconv.Atoi(parts[1])
		return n, parts[3]
	}
	return 0, ""
}

// targetName is the release asset name for the running platform. 必须与 CI
// 矩阵的 target 字段逐字一致,不做任何平台特殊映射,否则该平台永远 404。
func (u *Updater) targetName() string {
	target := runtime.GOOS + "-" + runtime.GOARCH
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return "betterocr-" + target + ext
}

func (u *Updater) download(ctx context.Context, cfg Config, release *releaseInfo) (string, error) {
	u.mu.Lock()
	u.status.State = "downloading"
	u.status.Progress = progressDownloadStart
	u.status.DownloadProgress = 0
	u.mu.Unlock()

	targetName := u.targetName()
	var binaryAsset, sha256Asset *assetInfo
	for i := range release.Assets {
		a := &release.Assets[i]
		switch a.Name {
		case targetName:
			binaryAsset = a
		case targetName + ".sha256":
			sha256Asset = a
		}
	}
	if binaryAsset == nil {
		return "", fmt.Errorf("no asset found for %s in release %s", targetName, release.TagName)
	}
	// 校验缺失时绝不降级安装:可选校验等于没有校验。
	if sha256Asset == nil {
		return "", fmt.Errorf("release %s is missing %s.sha256, refusing unverified update", release.TagName, targetName)
	}

	updateDir := filepath.Join(u.dataDir(), "updates")
	if err := os.MkdirAll(updateDir, 0o700); err != nil {
		return "", fmt.Errorf("create update dir: %w", err)
	}

	finalName := "betterocr-" + sanitizePathPart(release.TagName)
	if runtime.GOOS == "windows" {
		finalName += ".exe"
	}
	tmpPath := filepath.Join(updateDir, finalName+".tmp")
	finalPath := filepath.Join(updateDir, finalName)

	dlCtx, cancelDownload := context.WithTimeout(ctx, downloadTimeout)
	defer cancelDownload()

	downloadURL := u.resolveDownloadURL(cfg, binaryAsset.BrowserDownloadURL)
	if err := u.downloadFile(dlCtx, downloadURL, tmpPath, binaryAsset.Size); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("download binary: %w", err)
	}

	u.mu.Lock()
	u.status.Progress = progressVerifyStart
	u.mu.Unlock()

	shaBody, err := u.fetchAsset(dlCtx, cfg, sha256Asset, 1024)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("fetch sha256: %w", err)
	}
	// CI 在文件所在目录内执行 sha256sum,内容形如 "<hash>  <裸文件名>"。
	parts := strings.Fields(strings.TrimSpace(string(shaBody)))
	if len(parts) == 0 {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("empty sha256 file")
	}
	expectedHash := parts[0]
	actualHash, err := fileSHA256(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("compute sha256: %w", err)
	}
	if !strings.EqualFold(actualHash, expectedHash) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("sha256 mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	u.logger.Printf("update: SHA256 verified for %s", release.TagName)

	u.mu.Lock()
	u.status.Progress = progressVerifyDone
	u.mu.Unlock()

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	u.logger.Printf("update: downloaded %s to %s", release.TagName, finalPath)
	return finalPath, nil
}

// resolveDownloadURL maps a GitHub browser_download_url onto the proxy mirror
// when the proxy source is selected; in direct mode the URL is used as-is.
func (u *Updater) resolveDownloadURL(cfg Config, browserURL string) string {
	if cfg.Source != SourceProxy {
		return browserURL
	}
	base := strings.TrimRight(cfg.ProxyBaseURL, "/")
	const ghPrefix = "https://github.com/"
	if !strings.HasPrefix(browserURL, ghPrefix) {
		return browserURL
	}
	path := strings.TrimPrefix(browserURL, ghPrefix)
	const relSegment = "/releases/download/"
	idx := strings.Index(path, relSegment)
	if idx < 0 {
		return browserURL
	}
	ownerRepo := path[:idx]
	tagAndAsset := path[idx+len(relSegment):]
	return base + "/download/" + ownerRepo + "/" + tagAndAsset
}

func (u *Updater) downloadFile(ctx context.Context, url, destPath string, expectedSize int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	totalSize := resp.ContentLength
	if totalSize <= 0 && expectedSize > 0 {
		totalSize = expectedSize
	}

	var written int64
	var lastProgress float64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := f.Write(buf[:n]); wErr != nil {
				return wErr
			}
			written += int64(n)
			if totalSize > 0 {
				progress := float64(written) / float64(totalSize) * 100
				if progress-lastProgress >= 1 || progress >= 100 {
					u.mu.Lock()
					u.status.DownloadProgress = clampProgress(progress)
					u.status.Progress = overallDownloadProgress(progress)
					u.mu.Unlock()
					lastProgress = progress
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	u.mu.Lock()
	u.status.DownloadProgress = progressComplete
	u.status.Progress = overallDownloadProgress(progressComplete)
	u.mu.Unlock()
	return nil
}

// httpGet fetches url and returns up to limit bytes of the body along with the
// status code. Network errors are returned; HTTP error statuses are not.
func (u *Updater) httpGet(ctx context.Context, url string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (u *Updater) fetchAsset(ctx context.Context, cfg Config, asset *assetInfo, limit int64) ([]byte, error) {
	body, status, err := u.httpGet(ctx, u.resolveDownloadURL(cfg, asset.BrowserDownloadURL), limit)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", asset.Name, status)
	}
	return body, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (u *Updater) applyUpdate(newBinaryPath, tag string) error {
	u.mu.Lock()
	u.status.State = "applying"
	u.status.Progress = progressApplying
	u.mu.Unlock()

	// 换二进制靠在安装目录里改名,先探一次可写性。等 BeforeExec 关掉监听之后
	// 才发现目录只读,一次本该只是报错的更新就变成了服务中断;Windows 更糟,
	// 换文件的脚本在进程退出后才跑,失败时没人再把服务拉起来。
	if err := checkInstallDirWritable(); err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		return u.applyUpdateWindows(newBinaryPath, tag)
	}
	return u.applyUpdateUnix(newBinaryPath, tag)
}

// executablePath 指向当前进程的可执行文件,测试里换成临时目录中的假二进制。
var executablePath = os.Executable

// checkInstallDirWritable 确认可执行文件所在目录能建文件,例如二进制装在
// root 拥有的目录里而服务以普通用户运行时,这里就会提前失败。
func checkInstallDirWritable() error {
	execPath, err := executablePath()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}
	dir := filepath.Dir(execPath)
	probe, err := os.CreateTemp(dir, ".betterocr-swap-probe-*")
	if err != nil {
		return fmt.Errorf("install directory %s is not writable: %w", dir, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("install directory %s is not writable: %w", dir, err)
	}
	return os.Remove(name)
}

// applyUpdateUnix swaps the binary in place and re-execs it, so the PID never
// changes and a supervisor never sees the restart. 已知限制:.bak 在 exec 前
// 删除,新版启动崩溃无法自动回滚。
func (u *Updater) applyUpdateUnix(newBinaryPath, tag string) error {
	if u.hooks.BeforeExec != nil {
		if err := u.hooks.BeforeExec(tag); err != nil {
			return fmt.Errorf("prepare restart: %w", err)
		}
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	backupPath := execPath + ".bak"
	if err := os.Rename(execPath, backupPath); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}
	if err := copyFile(newBinaryPath, execPath); err != nil {
		_ = os.Rename(backupPath, execPath)
		return fmt.Errorf("install new binary: %w", err)
	}
	if err := os.Chmod(execPath, 0o755); err != nil {
		_ = os.Rename(backupPath, execPath)
		_ = os.Remove(newBinaryPath)
		return fmt.Errorf("chmod new binary: %w", err)
	}

	_ = os.Remove(backupPath)
	_ = os.Remove(newBinaryPath)

	u.logger.Printf("update: restarting with new binary %s", tag)
	u.mu.Lock()
	u.status.Progress = progressComplete
	u.mu.Unlock()
	return replaceProcess(execPath, os.Args, os.Environ())
}

// applyUpdateWindows cannot replace a running executable, so it hands the swap
// to a PowerShell script that waits for this process to exit first.
func (u *Updater) applyUpdateWindows(newBinaryPath, tag string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	execPath, err = filepath.Abs(execPath)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}

	updateDir := filepath.Dir(newBinaryPath)
	scriptPath := filepath.Join(updateDir, "apply-"+sanitizePathPart(tag)+".ps1")
	backupPath := execPath + ".bak"
	script := strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		fmt.Sprintf("$pidToWait = %d", os.Getpid()),
		"$exe = " + psQuote(execPath),
		"$new = " + psQuote(newBinaryPath),
		"$bak = " + psQuote(backupPath),
		"$argsList = " + psArray(os.Args[1:]),
		"$workDir = " + psQuote(cwd),
		"while (Get-Process -Id $pidToWait -ErrorAction SilentlyContinue) { Start-Sleep -Milliseconds 250 }",
		"if (Test-Path $bak) { Remove-Item -Force $bak }",
		"if (Test-Path $exe) { Move-Item -Force $exe $bak }",
		"Copy-Item -Force $new $exe",
		"Remove-Item -Force $new",
		"Start-Process -FilePath $exe -ArgumentList $argsList -WorkingDirectory $workDir",
		"Remove-Item -Force $PSCommandPath",
		"",
	}, "\r\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return fmt.Errorf("write apply script: %w", err)
	}

	if u.hooks.BeforeExec != nil {
		if err := u.hooks.BeforeExec(tag); err != nil {
			return fmt.Errorf("prepare restart: %w", err)
		}
	}

	proc, err := os.StartProcess("powershell.exe", []string{
		"powershell.exe",
		"-NoProfile",
		"-ExecutionPolicy", "Bypass",
		"-File", scriptPath,
	}, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Env:   os.Environ(),
	})
	if err != nil {
		return fmt.Errorf("start apply script: %w", err)
	}
	_ = proc.Release()

	u.logger.Printf("update: restarting with new binary %s", tag)
	u.mu.Lock()
	u.status.Progress = progressComplete
	u.mu.Unlock()
	os.Exit(0)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// normalizeConfig 是配置默认值的最后一层兜底:updater 不信任调用方。
func normalizeConfig(cfg Config) Config {
	cfg.Channel = strings.ToLower(strings.TrimSpace(cfg.Channel))
	if cfg.Channel == "" {
		cfg.Channel = "stable"
	} else if cfg.Channel != "stable" {
		cfg.Channel = "dev"
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = DefaultCheckInterval
	}
	cfg.Source = strings.ToLower(strings.TrimSpace(cfg.Source))
	if cfg.Source != SourceProxy {
		cfg.Source = SourceGitHub
	}
	cfg.ProxyBaseURL = strings.TrimRight(strings.TrimSpace(cfg.ProxyBaseURL), "/")
	if cfg.ProxyBaseURL == "" {
		cfg.ProxyBaseURL = DefaultProxyBaseURL
	}
	cfg.Repo = strings.Trim(strings.TrimSpace(cfg.Repo), "/")
	if cfg.Repo == "" {
		cfg.Repo = DefaultRepo
	}
	return cfg
}

// Normalize exposes the default fallbacks so the configuration file and the
// /api/version response report exactly what the updater will use.
func Normalize(cfg Config) Config { return normalizeConfig(cfg) }

func sanitizePathPart(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "update"
	}
	return b.String()
}

func psQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func psArray(values []string) string {
	if len(values) == 0 {
		return "@()"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, psQuote(value))
	}
	return "@(" + strings.Join(quoted, ", ") + ")"
}
