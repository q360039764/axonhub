package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultInstallDir   = `E:\AIApp\AxonHub`
	defaultVersionLabel = "desktop-q360039764"
	defaultHealthPort   = 8090
)

type appConfig struct {
	repoRoot       string
	installDir     string
	versionLabel   string
	goExe          string
	nodeDir        string
	pnpmExe        string
	noStart        bool
	checkOnly      bool
	keepBuild      bool
	keepStaticDist bool
	timeout        time.Duration
}

type toolchain struct {
	// goExe 是用于构建 axonhub.exe 的 Go 编译器路径。
	goExe string
	// goRoot 是 Go SDK 根目录，使用内置工具链时需要写入 GOROOT。
	goRoot string
	// nodeDir 是 Node、pnpm 所在目录，会被放在 PATH 最前面。
	nodeDir string
	// pnpmExe 是用于构建前端的 pnpm 命令路径。
	pnpmExe string
}

type buildOutput struct {
	// stageDir 是本次构建产物目录，后续替换安装目录时只从这里取文件。
	stageDir string
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() appConfig {
	var cfg appConfig
	flag.StringVar(&cfg.repoRoot, "repo", "", "AxonHub repository root. Defaults to searching upward from this exe/current directory.")
	flag.StringVar(&cfg.installDir, "install-dir", defaultInstallDir, "Installed AxonHub directory to update.")
	flag.StringVar(&cfg.versionLabel, "version-label", defaultVersionLabel, "Version label written into the rebuilt axonhub.exe.")
	flag.StringVar(&cfg.goExe, "go", "", "Optional go.exe path.")
	flag.StringVar(&cfg.nodeDir, "node-dir", "", "Optional directory containing node.exe and pnpm.cmd.")
	flag.StringVar(&cfg.pnpmExe, "pnpm", "", "Optional pnpm.cmd path.")
	flag.BoolVar(&cfg.noStart, "no-start", false, "Replace files but do not start desktop.bat.")
	flag.BoolVar(&cfg.checkOnly, "check", false, "Only validate paths and toolchain, then exit.")
	flag.BoolVar(&cfg.keepBuild, "keep-build", false, "Keep .builds/desktop-auto after successful update.")
	flag.BoolVar(&cfg.keepStaticDist, "keep-static-dist", false, "Keep internal/server/static/dist files after backend build.")
	flag.DurationVar(&cfg.timeout, "timeout", 90*time.Second, "Startup health check timeout.")
	flag.Parse()
	return cfg
}

func run(cfg appConfig) error {
	if runtime.GOOS != "windows" {
		return errors.New("desktop updater only supports Windows")
	}

	repoRoot, err := resolveRepoRoot(cfg.repoRoot)
	if err != nil {
		return err
	}
	cfg.repoRoot = repoRoot

	installDir, err := filepath.Abs(cfg.installDir)
	if err != nil {
		return fmt.Errorf("resolve install dir: %w", err)
	}
	cfg.installDir = installDir

	if err := validateRepo(repoRoot); err != nil {
		return err
	}
	if err := validateInstallDir(installDir); err != nil {
		return err
	}

	tc, err := detectToolchain(cfg)
	if err != nil {
		return err
	}

	branch := gitOutput(repoRoot, "branch", "--show-current")
	commit := gitOutput(repoRoot, "rev-parse", "HEAD")
	if commit == "" {
		commit = "unknown"
	}

	logInfo("Repo: %s", repoRoot)
	logInfo("Branch: %s", emptyDefault(branch, "unknown"))
	logInfo("Commit: %s", commit)
	logInfo("Install dir: %s", installDir)
	logInfo("Version label: %s", cfg.versionLabel)
	logInfo("Go: %s", tc.goExe)
	logInfo("Node dir: %s", tc.nodeDir)
	logInfo("pnpm: %s", tc.pnpmExe)

	if cfg.checkOnly {
		logSuccess("Environment check completed.")
		return nil
	}

	output, err := buildArtifacts(cfg, tc, commit)
	if err != nil {
		return err
	}
	if !cfg.keepBuild {
		defer os.RemoveAll(filepath.Dir(output.stageDir))
	}

	if err := stopInstalledApp(installDir); err != nil {
		return err
	}
	backupDir, err := backupAndReplace(installDir, output.stageDir, repoRoot, cfg.versionLabel)
	if err != nil {
		return err
	}
	logSuccess("Backup created: %s", backupDir)

	if cfg.noStart {
		logSuccess("Update completed. Start skipped by --no-start.")
		return nil
	}

	if err := startDesktop(installDir); err != nil {
		return err
	}
	if err := verifyDesktop(installDir, cfg.versionLabel, cfg.timeout); err != nil {
		return err
	}

	logSuccess("Desktop version is ready: %s", cfg.versionLabel)
	return nil
}

func resolveRepoRoot(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}

	var starts []string
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}

	for _, start := range starts {
		if root, ok := findRepoRoot(start); ok {
			return root, nil
		}
	}
	return "", errors.New("cannot locate repository root; pass --repo")
}

func findRepoRoot(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		if fileExists(filepath.Join(dir, "go.mod")) &&
			fileExists(filepath.Join(dir, "frontend", "package.json")) &&
			fileExists(filepath.Join(dir, "deploy", "build-desktop.ps1")) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func validateRepo(repoRoot string) error {
	required := []string{
		"go.mod",
		filepath.Join("cmd", "axonhub"),
		filepath.Join("frontend", "package.json"),
		filepath.Join("deploy", "desktop.bat"),
		filepath.Join("deploy", "build-desktop.ps1"),
		filepath.Join("deploy", "desktop", "AxonHubDesktop.cs"),
	}
	for _, rel := range required {
		if !exists(filepath.Join(repoRoot, rel)) {
			return fmt.Errorf("required repository path missing: %s", rel)
		}
	}
	return nil
}

func validateInstallDir(installDir string) error {
	if !dirExists(installDir) {
		return fmt.Errorf("install directory does not exist: %s", installDir)
	}
	if !fileExists(filepath.Join(installDir, "config.yml")) {
		logWarn("config.yml not found in install dir; AxonHub may start with defaults.")
	}
	return nil
}

func detectToolchain(cfg appConfig) (toolchain, error) {
	var tc toolchain

	goExe := cfg.goExe
	if goExe == "" {
		goExe = firstExistingFile(
			filepath.Join(filepath.Dir(cfg.repoRoot), ".tools", "go", "bin", "go.exe"),
			filepath.Join(filepath.Dir(filepath.Dir(cfg.repoRoot)), ".tools", "go", "bin", "go.exe"),
		)
	}
	if goExe == "" {
		if found, err := exec.LookPath("go"); err == nil {
			goExe = found
		}
	}
	if goExe == "" {
		return tc, errors.New("go.exe not found; pass --go or install Go")
	}
	tc.goExe = goExe
	if strings.HasSuffix(strings.ToLower(goExe), `\bin\go.exe`) {
		tc.goRoot = filepath.Dir(filepath.Dir(goExe))
	}

	nodeDir := cfg.nodeDir
	if nodeDir == "" {
		nodeDir = detectNodeDir(cfg.repoRoot)
	}
	if nodeDir == "" {
		return tc, errors.New("node directory not found; pass --node-dir")
	}
	tc.nodeDir = nodeDir

	pnpmExe := cfg.pnpmExe
	if pnpmExe == "" {
		pnpmExe = firstExistingFile(filepath.Join(nodeDir, "pnpm.cmd"))
	}
	if pnpmExe == "" {
		if found, err := exec.LookPath("pnpm"); err == nil {
			pnpmExe = found
		}
	}
	if pnpmExe == "" {
		return tc, errors.New("pnpm not found; pass --pnpm or enable corepack")
	}
	tc.pnpmExe = pnpmExe

	return tc, nil
}

func detectNodeDir(repoRoot string) string {
	toolRootCandidates := []string{
		filepath.Join(filepath.Dir(repoRoot), ".tools"),
		filepath.Join(filepath.Dir(filepath.Dir(repoRoot)), ".tools"),
	}
	for _, toolRoot := range toolRootCandidates {
		if !dirExists(toolRoot) {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(toolRoot, "node-v*-win-x64"))
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		for _, match := range matches {
			if fileExists(filepath.Join(match, "node.exe")) && fileExists(filepath.Join(match, "pnpm.cmd")) {
				return match
			}
		}
	}

	if found, err := exec.LookPath("node"); err == nil {
		dir := filepath.Dir(found)
		if fileExists(filepath.Join(dir, "pnpm.cmd")) {
			return dir
		}
	}
	return ""
}

func buildArtifacts(cfg appConfig, tc toolchain, commit string) (buildOutput, error) {
	autoRoot := filepath.Join(cfg.repoRoot, ".builds", "desktop-auto")
	stageDir := filepath.Join(autoRoot, "stage")
	if err := os.RemoveAll(autoRoot); err != nil {
		return buildOutput{}, fmt.Errorf("clean build directory: %w", err)
	}
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return buildOutput{}, fmt.Errorf("create stage directory: %w", err)
	}

	if err := buildFrontend(cfg, tc); err != nil {
		return buildOutput{}, err
	}
	if err := syncStaticDist(cfg.repoRoot); err != nil {
		return buildOutput{}, err
	}

	if err := buildBackend(cfg, tc, commit, filepath.Join(stageDir, "axonhub.exe")); err != nil {
		return buildOutput{}, err
	}

	if !cfg.keepStaticDist {
		if err := restoreStaticDist(cfg.repoRoot); err != nil {
			return buildOutput{}, err
		}
	}

	if err := buildDesktopShell(cfg.repoRoot, stageDir); err != nil {
		return buildOutput{}, err
	}

	required := []string{
		"axonhub.exe",
		"AxonHubDesktop.exe",
		"Microsoft.Web.WebView2.Core.dll",
		"Microsoft.Web.WebView2.WinForms.dll",
		"WebView2Loader.dll",
	}
	for _, name := range required {
		if !fileExists(filepath.Join(stageDir, name)) {
			return buildOutput{}, fmt.Errorf("build output missing: %s", name)
		}
	}
	return buildOutput{stageDir: stageDir}, nil
}

func buildFrontend(cfg appConfig, tc toolchain) error {
	logInfo("Building frontend...")
	env := withPrependedPath(os.Environ(), tc.nodeDir)
	return runCommand("frontend build", tc.pnpmExe, []string{"vite", "build"}, filepath.Join(cfg.repoRoot, "frontend"), env)
}

func syncStaticDist(repoRoot string) error {
	logInfo("Syncing frontend dist into backend static embed directory...")
	source := filepath.Join(repoRoot, "frontend", "dist")
	target := filepath.Join(repoRoot, "internal", "server", "static", "dist")
	if !dirExists(source) {
		return fmt.Errorf("frontend dist missing: %s", source)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("clean backend static dist: %w", err)
	}
	return copyDir(source, target)
}

func restoreStaticDist(repoRoot string) error {
	target := filepath.Join(repoRoot, "internal", "server", "static", "dist")
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("restore static dist: %w", err)
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		return fmt.Errorf("create static dist placeholder dir: %w", err)
	}
	return os.WriteFile(filepath.Join(target, ".gitkeep"), nil, 0644)
}

func buildBackend(cfg appConfig, tc toolchain, commit string, output string) error {
	logInfo("Building backend: %s", output)
	buildTime := time.Now().Format(time.RFC3339)
	ldflags := fmt.Sprintf(
		"-s -w -X github.com/looplj/axonhub/internal/build.Version=%s -X github.com/looplj/axonhub/internal/build.Commit=%s -X github.com/looplj/axonhub/internal/build.BuildTime=%s",
		cfg.versionLabel,
		commit,
		buildTime,
	)
	env := os.Environ()
	env = appendOrReplaceEnv(env, "GOPATH", filepath.Join(filepath.Dir(filepath.Dir(tc.goExe)), "..", "gopath"))
	env = appendOrReplaceEnv(env, "GOCACHE", filepath.Join(filepath.Dir(filepath.Dir(tc.goExe)), "..", "gocache"))
	if tc.goRoot != "" {
		env = appendOrReplaceEnv(env, "GOROOT", tc.goRoot)
	}
	env = withPrependedPath(env, filepath.Dir(tc.goExe))
	return runCommand("backend build", tc.goExe, []string{"build", "-ldflags", ldflags, "-tags=nomsgpack", "-o", output, "./cmd/axonhub"}, cfg.repoRoot, env)
}

func buildDesktopShell(repoRoot string, outputDir string) error {
	logInfo("Building desktop shell...")
	script := filepath.Join(repoRoot, "deploy", "build-desktop.ps1")
	return runCommand(
		"desktop shell build",
		"powershell.exe",
		[]string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-OutputDir", outputDir},
		repoRoot,
		os.Environ(),
	)
}

func stopInstalledApp(installDir string) error {
	logInfo("Stopping installed AxonHub processes if present...")
	axonhubPath := filepath.Join(installDir, "axonhub.exe")
	desktopPath := filepath.Join(installDir, "AxonHubDesktop.exe")
	command := fmt.Sprintf(
		"$targets = @(%s, %s); Get-Process -Name 'axonhub','AxonHubDesktop' -ErrorAction SilentlyContinue | Where-Object { $_.Path -and ($targets -contains $_.Path) } | Stop-Process -Force",
		psQuote(axonhubPath),
		psQuote(desktopPath),
	)
	if err := runCommand("stop installed app", "powershell.exe", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", command}, installDir, os.Environ()); err != nil {
		return err
	}
	time.Sleep(1200 * time.Millisecond)
	return nil
}

func backupAndReplace(installDir, stageDir, repoRoot, versionLabel string) (string, error) {
	logInfo("Backing up and replacing installed files...")
	backupDir := filepath.Join(installDir, "beifen", versionLabel+"-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	backupFiles := []string{
		"axonhub.exe",
		"AxonHubDesktop.exe",
		"Microsoft.Web.WebView2.Core.dll",
		"Microsoft.Web.WebView2.WinForms.dll",
		"WebView2Loader.dll",
		"desktop.bat",
		"build-desktop.ps1",
		"ensure-axonhub.ps1",
		"start-hidden.vbs",
	}
	for _, name := range backupFiles {
		src := filepath.Join(installDir, name)
		if fileExists(src) {
			if err := copyFile(src, filepath.Join(backupDir, name)); err != nil {
				return "", err
			}
		}
	}
	if dirExists(filepath.Join(installDir, "desktop")) {
		if err := copyDir(filepath.Join(installDir, "desktop"), filepath.Join(backupDir, "desktop")); err != nil {
			return "", err
		}
	}

	stageFiles := []string{
		"axonhub.exe",
		"AxonHubDesktop.exe",
		"Microsoft.Web.WebView2.Core.dll",
		"Microsoft.Web.WebView2.WinForms.dll",
		"WebView2Loader.dll",
	}
	for _, name := range stageFiles {
		if err := copyFile(filepath.Join(stageDir, name), filepath.Join(installDir, name)); err != nil {
			return "", err
		}
	}

	deployFiles := []string{"desktop.bat", "build-desktop.ps1", "ensure-axonhub.ps1", "start-hidden.vbs"}
	for _, name := range deployFiles {
		if err := copyFile(filepath.Join(repoRoot, "deploy", name), filepath.Join(installDir, name)); err != nil {
			return "", err
		}
	}
	if err := copyDir(filepath.Join(repoRoot, "deploy", "desktop"), filepath.Join(installDir, "desktop")); err != nil {
		return "", err
	}

	return backupDir, nil
}

func startDesktop(installDir string) error {
	logInfo("Starting desktop.bat...")
	desktopBat := filepath.Join(installDir, "desktop.bat")
	cmd := exec.Command("cmd.exe", "/c", "start", "", desktopBat)
	cmd.Dir = installDir
	return cmd.Start()
}

func verifyDesktop(installDir, versionLabel string, timeout time.Duration) error {
	port := detectConfiguredPort(installDir)
	healthURL := fmt.Sprintf("http://localhost:%d/health", port)
	logInfo("Waiting for health check: %s", healthURL)

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		body, err := httpGet(healthURL)
		if err == nil {
			if strings.Contains(body, versionLabel) {
				logSuccess("Health check OK: %s", healthURL)
				return nil
			}
			lastErr = fmt.Errorf("health response does not contain version label %q: %s", versionLabel, body)
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("desktop health check failed: %w", lastErr)
}

func detectConfiguredPort(installDir string) int {
	exe := filepath.Join(installDir, "axonhub.exe")
	cmd := exec.Command(exe, "config", "get", "server.port")
	cmd.Dir = installDir
	out, err := cmd.Output()
	if err != nil {
		return defaultHealthPort
	}
	value := strings.TrimSpace(string(out))
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 {
		return defaultHealthPort
	}
	return port
}

func httpGet(url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(data))
	}
	return string(data), nil
}

func runCommand(label, name string, args []string, dir string, env []string) error {
	logInfo("%s: %s %s", label, name, strings.Join(args, " "))
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", label, err)
	}
	return nil
}

func gitOutput(repoRoot string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source file %s: %w", src, err)
	}
	defer source.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create target dir for %s: %w", dst, err)
	}
	target, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create target file %s: %w", dst, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close target file %s: %w", dst, err)
	}
	if info, err := os.Stat(src); err == nil {
		_ = os.Chmod(dst, info.Mode())
	}
	return nil
}

func copyDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clean target dir %s: %w", dst, err)
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return fmt.Errorf("create target dir %s: %w", dst, err)
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		return copyFile(path, target)
	})
}

func withPrependedPath(env []string, dir string) []string {
	pathValue := dir + string(os.PathListSeparator) + os.Getenv("Path")
	return appendOrReplaceEnv(env, "Path", pathValue)
}

func appendOrReplaceEnv(env []string, key string, value string) []string {
	prefix := strings.ToUpper(key) + "="
	for i, item := range env {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

func firstExistingFile(paths ...string) string {
	for _, path := range paths {
		if fileExists(path) {
			return path
		}
	}
	return ""
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func emptyDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func psQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func logInfo(format string, args ...any) {
	fmt.Printf("[INFO] "+format+"\n", args...)
}

func logWarn(format string, args ...any) {
	fmt.Printf("[WARN] "+format+"\n", args...)
}

func logSuccess(format string, args ...any) {
	fmt.Printf("[SUCCESS] "+format+"\n", args...)
}
