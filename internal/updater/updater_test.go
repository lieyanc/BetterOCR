package updater

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lieyanc/BetterOCR/internal/version"
)

// TestCheckOnlyAcrossSourcesAndChannels covers release selection over both
// download sources (proxy mirror, GitHub direct) and both channels (stable,
// dev), including the dev-channel same-commit skip.
func TestCheckOnlyAcrossSourcesAndChannels(t *testing.T) {
	cases := []struct {
		name         string
		cfg          Config
		localVersion string
		localCommit  string
		remote       releaseVersionInfo
		direct       bool // serve the GitHub-direct layout instead of the proxy API
		wantUpdate   bool
		wantLatest   string
	}{
		{
			name:         "proxy stable selects newest stable release",
			cfg:          Config{Channel: "stable", Source: "proxy", Repo: "owner/repo"},
			localVersion: "v1.0.0",
			remote:       releaseVersionInfo{Tag: "v1.4.0"},
			wantUpdate:   true,
			wantLatest:   "v1.4.0",
		},
		{
			name:         "proxy stable ignores older stable release",
			cfg:          Config{Channel: "stable", Source: "proxy", Repo: "owner/repo"},
			localVersion: "v1.4.0",
			remote:       releaseVersionInfo{Tag: "v1.3.9"},
			wantUpdate:   false,
			wantLatest:   "v1.3.9",
		},
		{
			name:         "proxy dev selects newest prerelease",
			cfg:          Config{Channel: "dev", Source: "proxy", Repo: "owner/repo"},
			localVersion: "dev-0007-20260401-aaaaaaa",
			localCommit:  "aaaaaaa",
			remote:       releaseVersionInfo{Version: "dev-0042-20260425-bbbbbbb", Commit: "bbbbbbb", BuildTime: "2026-04-25T00:00:00Z", Tag: "dev"},
			wantUpdate:   true,
			wantLatest:   "dev-0042-20260425-bbbbbbb",
		},
		{
			name:         "proxy dev skips release for same commit",
			cfg:          Config{Channel: "dev", Source: "proxy", Repo: "owner/repo"},
			localVersion: "dev-0042-20260425-bbbbbbb",
			localCommit:  "bbbbbbb",
			remote:       releaseVersionInfo{Version: "dev-0042-20260425-bbbbbbb", Commit: "bbbbbbb", BuildTime: "2026-04-25T00:00:00Z", Tag: "dev"},
			wantUpdate:   false,
			wantLatest:   "dev-0042-20260425-bbbbbbb",
		},
		{
			name:         "github direct dev reads pinned dev metadata",
			cfg:          Config{Channel: "dev", Repo: "owner/repo"},
			localVersion: "dev-0007-20260401-aaaaaaa",
			localCommit:  "aaaaaaa",
			remote:       releaseVersionInfo{Version: "dev-0042-20260425-bbbbbbb", Commit: "bbbbbbb", BuildTime: "2026-04-25T00:00:00Z", Tag: "dev"},
			direct:       true,
			wantUpdate:   true,
			wantLatest:   "dev-0042-20260425-bbbbbbb",
		},
		{
			name:         "github direct stable follows the latest redirect",
			cfg:          Config{Channel: "stable", Repo: "owner/repo"},
			localVersion: "v1.0.0",
			remote:       releaseVersionInfo{Version: "v1.4.0", Commit: "bbbbbbb", BuildTime: "2026-04-25T00:00:00Z", Tag: "v1.4.0"},
			direct:       true,
			wantUpdate:   true,
			wantLatest:   "v1.4.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setTestVersion(t, tc.localVersion, tc.localCommit)
			cfg := tc.cfg
			if tc.direct {
				metadata, err := json.Marshal(tc.remote)
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/owner/repo/releases/download/" + tc.remote.Tag + "/version.json",
						"/owner/repo/releases/latest/download/version.json":
						_, _ = w.Write(metadata)
					default:
						t.Errorf("unexpected path: %s", r.URL.Path)
						w.WriteHeader(http.StatusInternalServerError)
					}
				}))
				defer server.Close()
				setTestGitHubBaseURL(t, server.URL)
			} else {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/releases/owner/repo/latest":
						_ = json.NewEncoder(w).Encode(releaseInfo{TagName: tc.remote.Tag, Assets: []assetInfo{}})
					case "/api/releases/owner/repo/dev":
						_ = json.NewEncoder(w).Encode(releaseInfo{
							TagName:         "dev",
							TargetCommitish: tc.remote.Commit,
							Prerelease:      true,
							Assets: []assetInfo{{
								Name:               "version.json",
								BrowserDownloadURL: "https://github.com/owner/repo/releases/download/dev/version.json",
							}},
						})
					case "/download/owner/repo/dev/version.json":
						_ = json.NewEncoder(w).Encode(tc.remote)
					default:
						t.Errorf("unexpected path: %s", r.URL.Path)
						w.WriteHeader(http.StatusInternalServerError)
					}
				}))
				defer server.Close()
				cfg.ProxyBaseURL = server.URL
			}

			result, err := testUpdater(cfg).CheckOnly(context.Background())
			if err != nil {
				t.Fatalf("CheckOnly returned error: %v", err)
			}
			if result.HasUpdate != tc.wantUpdate || result.LatestVersion != tc.wantLatest {
				t.Fatalf("CheckOnly = %+v, want update=%v latest=%q", result, tc.wantUpdate, tc.wantLatest)
			}
		})
	}
}

func TestCheckOnlyReportsNoReleaseWithoutError(t *testing.T) {
	setTestVersion(t, "v1.0.0", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	setTestGitHubBaseURL(t, server.URL)

	result, err := testUpdater(Config{Channel: "stable", Repo: "owner/repo"}).CheckOnly(context.Background())
	if err != nil {
		t.Fatalf("CheckOnly returned error: %v", err)
	}
	if result.HasUpdate || result.LatestVersion != "" {
		t.Fatalf("CheckOnly = %+v, want no update for an empty repository", result)
	}
}

func TestCheckOnlyClearsPreviousFailure(t *testing.T) {
	setTestVersion(t, "v1.0.0", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.4.0","assets":[]}`))
	}))
	defer server.Close()

	u := testUpdater(Config{Channel: "stable", Source: "proxy", ProxyBaseURL: server.URL, Repo: "owner/repo"})
	u.setError("download failed: fixture")
	if status := u.Status(); status.State != "failed" || status.Error == "" {
		t.Fatalf("precondition not met: %+v", status)
	}

	if _, err := u.CheckOnly(context.Background()); err != nil {
		t.Fatalf("CheckOnly returned error: %v", err)
	}
	if status := u.Status(); status.State != "idle" || status.Error != "" {
		t.Fatalf("status after successful check = %+v, want cleared idle", status)
	}
}

func TestPerformUpdateDownloadsAndVerifiesPrerelease(t *testing.T) {
	setTestVersion(t, "dev-0007-20260401-aaaaaaa", "aaaaaaa")

	cfg := Config{Channel: "dev", Source: "proxy", Repo: "owner/repo"}
	dataDir := t.TempDir()
	u := New(
		func() Config { return cfg },
		func() string { return dataDir },
		log.New(io.Discard, "", 0),
		RestartHooks{},
	)

	const tag = "dev"
	const remoteVersion = "dev-0042-20260425-bbbbbbb"
	targetName := u.targetName()
	binary := []byte("new binary")
	sum := fmt.Sprintf("%x", sha256.Sum256(binary))
	shaContent := sum + "  " + targetName + "\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/owner/repo/dev":
			_ = json.NewEncoder(w).Encode(releaseInfo{
				TagName:         tag,
				TargetCommitish: "bbbbbbb",
				Prerelease:      true,
				Assets:          proxyAssets(tag, targetName, int64(len(binary))),
			})
		case "/download/owner/repo/" + tag + "/" + targetName:
			_, _ = w.Write(binary)
		case "/download/owner/repo/" + tag + "/" + targetName + ".sha256":
			_, _ = w.Write([]byte(shaContent))
		case "/download/owner/repo/" + tag + "/version.json":
			_ = json.NewEncoder(w).Encode(releaseVersionInfo{
				Version: remoteVersion, Commit: "bbbbbbb", BuildTime: "2026-04-25T00:00:00Z", Tag: tag,
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	cfg.ProxyBaseURL = server.URL

	u.performUpdate(context.Background())

	status := u.Status()
	if status.State != "ready" {
		t.Fatalf("expected update to be ready, got %q: %s", status.State, status.Error)
	}
	if status.LatestVersion != remoteVersion || u.pendingTag != tag {
		t.Fatalf("expected pending latest tag %q, got status=%q pending=%q", tag, status.LatestVersion, u.pendingTag)
	}
	if status.Progress != progressVerifyDone {
		t.Fatalf("expected overall progress %d, got %.0f", progressVerifyDone, status.Progress)
	}
	got, err := os.ReadFile(u.pendingBinaryPath)
	if err != nil {
		t.Fatalf("read pending binary: %v", err)
	}
	if string(got) != string(binary) {
		t.Fatalf("pending binary content mismatch")
	}
}

// TestPerformUpdateRejectsUnverifiableBinary keeps the checksum mandatory:
// 缺失或不匹配都必须硬失败,不能降级安装。
func TestPerformUpdateRejectsUnverifiableBinary(t *testing.T) {
	cases := []struct {
		name      string
		omitSHA   bool
		shaOfText string
		wantError string
	}{
		{name: "missing sha256 asset", omitSHA: true, wantError: "sha256"},
		{name: "sha256 mismatch", shaOfText: "different bytes", wantError: "mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setTestVersion(t, "v1.0.0", "")

			cfg := Config{Channel: "stable", Source: "proxy", Repo: "owner/repo"}
			dataDir := t.TempDir()
			u := New(
				func() Config { return cfg },
				func() string { return dataDir },
				log.New(io.Discard, "", 0),
				RestartHooks{},
			)

			const tag = "v1.4.0"
			targetName := u.targetName()
			binary := []byte("new binary")
			shaSource := tc.shaOfText
			if shaSource == "" {
				shaSource = string(binary)
			}
			shaContent := fmt.Sprintf("%x", sha256.Sum256([]byte(shaSource))) + "  " + targetName + "\n"

			assets := proxyAssets(tag, targetName, int64(len(binary)))
			if tc.omitSHA {
				assets = assets[:1]
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/releases/owner/repo/latest":
					_ = json.NewEncoder(w).Encode(releaseInfo{TagName: tag, Assets: assets})
				case "/download/owner/repo/" + tag + "/" + targetName:
					_, _ = w.Write(binary)
				case "/download/owner/repo/" + tag + "/" + targetName + ".sha256":
					_, _ = w.Write([]byte(shaContent))
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			cfg.ProxyBaseURL = server.URL

			u.performUpdate(context.Background())

			status := u.Status()
			if status.State != "failed" || !strings.Contains(status.Error, tc.wantError) {
				t.Fatalf("expected failure containing %q, got state=%q error=%q", tc.wantError, status.State, status.Error)
			}
			// 拒绝路径必须不留残留:目录可能压根没建(资产缺失时提前退出)。
			entries, err := os.ReadDir(dataDir + "/updates")
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("expected rejected download to be removed, found %d file(s)", len(entries))
			}
		})
	}
}

func TestApplyPendingMovesToApplyingBeforeAsyncRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	u := testUpdater(Config{})
	u.bgCtx = ctx
	// 取消掉的 bgCtx 加上返回错误的 BeforeExec,避免测试里真的 exec。
	u.hooks.BeforeExec = func(string) error { return context.Canceled }
	u.status.State = "ready"
	u.pendingBinaryPath = t.TempDir() + "/betterocr-new"
	u.pendingTag = "v1.2.0"

	if err := u.ApplyPending(context.Background()); err != nil {
		t.Fatalf("ApplyPending returned error: %v", err)
	}

	status := u.Status()
	if status.State != "applying" {
		t.Fatalf("expected state applying immediately, got %q", status.State)
	}
	if status.Progress != progressApplying {
		t.Fatalf("expected applying progress %d, got %.0f", progressApplying, status.Progress)
	}
	if u.pendingBinaryPath != "" || u.pendingTag != "" {
		t.Fatalf("expected pending update to be consumed")
	}

	err := u.ApplyPending(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no pending update") {
		t.Fatalf("expected duplicate apply to be rejected, got %v", err)
	}

	time.Sleep(250 * time.Millisecond)
}

func TestDismissPendingRemovesDownloadedBinary(t *testing.T) {
	u := testUpdater(Config{})
	pending := t.TempDir() + "/betterocr-dev"
	if err := os.WriteFile(pending, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	u.status.State = "ready"
	u.status.LatestVersion = "dev-0042-20260425-bbbbbbb"
	u.pendingBinaryPath = pending
	u.pendingTag = "dev"

	u.DismissPending()

	if state := u.Status().State; state != "idle" {
		t.Fatalf("state after dismiss = %q, want idle", state)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("expected pending binary to be deleted, stat err = %v", err)
	}
}

func TestWaitForIdleStopsWhenApplicationContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u := New(
		func() Config { return Config{} },
		func() string { return "" },
		log.New(io.Discard, "", 0),
		RestartHooks{IsBusy: func() bool { return true }},
	)

	if err := u.waitForIdle(ctx); err != context.Canceled {
		t.Fatalf("waitForIdle error = %v, want context.Canceled", err)
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	got := Normalize(Config{Channel: " DEV ", Source: "Proxy", ProxyBaseURL: "https://mirror.example/", Repo: " owner/repo/ "})
	if got.Channel != "dev" || got.Source != SourceProxy || got.ProxyBaseURL != "https://mirror.example" || got.Repo != "owner/repo" {
		t.Fatalf("Normalize = %+v", got)
	}
	if got.CheckInterval != DefaultCheckInterval {
		t.Fatalf("CheckInterval = %d, want %d", got.CheckInterval, DefaultCheckInterval)
	}

	defaults := Normalize(Config{Channel: "anything-else"})
	if defaults.Channel != "dev" || defaults.Source != SourceGitHub ||
		defaults.Repo != DefaultRepo || defaults.ProxyBaseURL != DefaultProxyBaseURL {
		t.Fatalf("Normalize defaults = %+v", defaults)
	}
	if stable := Normalize(Config{}); stable.Channel != "stable" {
		t.Fatalf("empty channel = %q, want stable", stable.Channel)
	}
}

func TestTargetNameMatchesReleaseAssetNaming(t *testing.T) {
	name := testUpdater(Config{}).targetName()
	if !strings.HasPrefix(name, "betterocr-") {
		t.Fatalf("targetName = %q, want betterocr- prefix matching the CI matrix", name)
	}
}

// proxyAssets mirrors the asset set published by CI for one release tag.
func proxyAssets(tag, targetName string, size int64) []assetInfo {
	const base = "https://github.com/owner/repo/releases/download/"
	return []assetInfo{
		{Name: targetName, BrowserDownloadURL: base + tag + "/" + targetName, Size: size},
		{Name: targetName + ".sha256", BrowserDownloadURL: base + tag + "/" + targetName + ".sha256"},
		{Name: "version.json", BrowserDownloadURL: base + tag + "/version.json"},
	}
}

func setTestVersion(t *testing.T, ver, commit string) {
	t.Helper()
	originalVersion, originalCommit := version.Version, version.Commit
	version.Version = ver
	if commit != "" {
		version.Commit = commit
	}
	t.Cleanup(func() { version.Version, version.Commit = originalVersion, originalCommit })
}

func setTestGitHubBaseURL(t *testing.T, url string) {
	t.Helper()
	original := githubBaseURL
	githubBaseURL = url
	t.Cleanup(func() { githubBaseURL = original })
}

func testUpdater(cfg Config) *Updater {
	return New(
		func() Config { return cfg },
		func() string { return "" },
		log.New(io.Discard, "", 0),
		RestartHooks{},
	)
}
