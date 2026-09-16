package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"workbuddy-helper/internal/client"
	"workbuddy-helper/internal/instance"
	"workbuddy-helper/internal/server"
	"workbuddy-helper/internal/service"
	"workbuddy-helper/internal/store"
)

//go:embed web/*
var webFiles embed.FS

// appVersion 由构建脚本通过 -ldflags 注入（如 1.0.2）
var appVersion = "dev"

func main() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "WorkBuddy Helper v%s — 腾讯 WorkBuddy/CodeBuddy 多账户签到与积分看板\n\n", appVersion)
		fmt.Fprintf(out, "用法:\n")
		fmt.Fprintf(out, "  workbuddy-helper [选项]                    # 启动本地服务\n")
		fmt.Fprintf(out, "  workbuddy-helper --export-portable DIR    # 导出当前数据为可移植副本(供 Docker 使用)\n")
		fmt.Fprintf(out, "  workbuddy-helper --import DIR             # 从 DIR 导入账号到数据目录(按 UID 去重)\n")
		fmt.Fprintf(out, "\n选项:\n")
		flag.PrintDefaults()
	}
	var (
		once           = flag.Bool("once", false, "run all enabled accounts once and exit")
		dataDir        = flag.String("data-dir", defaultDataDir(), "credential and state directory")
		noOpen         = flag.Bool("no-open", false, "do not open the browser")
		port           = flag.Int("port", 0, "local port (0 chooses an available port)")
		host           = flag.String("host", defaultHost(), "listen address (use 0.0.0.0 only behind a trusted firewall)")
		exportPortable = flag.String("export-portable", "", "export current data as portable key-file copy into DIR, then exit")
		importDir      = flag.String("import", "", "import accounts from DIR (state.bin) into the data directory, then exit")
	)
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	server.SetVersion(appVersion)

	if *exportPortable != "" {
		runExport(defaultExportSource(*dataDir), *exportPortable)
		return
	}
	if *importDir != "" {
		runImport(*importDir, *dataDir)
		return
	}

	st := store.New(*dataDir)
	instanceLock, err := instance.Acquire(*dataDir)
	if err != nil {
		fatal("%v", err)
	}
	defer instanceLock.Close()
	svc, err := service.New(st, client.New())
	if err != nil {
		fatal("无法加载本地数据: %v", err)
	}
	defer svc.StopScheduler()

	if *once {
		// --once 按每日任务版本执行（CLI 无视图概念）
		results := svc.RunAll("")
		failed := 0
		for _, a := range results {
			fmt.Printf("%-20s %-14s balance=%d %s\n", a.DisplayName, a.Status, a.Balance, a.LastError)
			if a.Status == "error" || a.Status == "needs_login" || a.Status == "rate_limited" {
				failed++
			}
		}
		if failed > 0 {
			os.Exit(2)
		}
		return
	}

	webRoot, err := fs.Sub(webFiles, "web")
	if err != nil {
		fatal("无法加载界面资源: %v", err)
	}

	addr := net.JoinHostPort(*host, fmt.Sprintf("%d", *port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fatal("无法启动本地服务: %v", err)
	}
	serverAddr := "http://" + listener.Addr().String()

	shutdownCh := make(chan struct{}, 1)
	appServer := &http.Server{
		Handler:           server.New(svc, webRoot, listener.Addr().String(), func() { shutdownCh <- struct{}{} }).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	svc.StartScheduler()
	go func() {
		if err := appServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server stopped: %v", err)
			shutdownCh <- struct{}{}
		}
	}()

	fmt.Printf("WorkBuddy Helper v%s 已启动: %s\n", appVersion, serverAddr)
	fmt.Printf("凭据加密存放于: %s\n", *dataDir)
	if !*noOpen {
		if err := openBrowser(serverAddr); err != nil {
			log.Printf("无法自动打开浏览器: %v", err)
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	select {
	case <-signals:
	case <-shutdownCh:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = appServer.Shutdown(ctx)
}

func runExport(srcFile, dstDir string) {
	if srcFile == "" {
		fatal("未找到可导出的本地数据(state.bin)")
	}
	if _, err := os.Stat(srcFile); err != nil {
		fatal("无法读取本地数据 %s: %v", srcFile, err)
	}
	n, err := store.ExportPortableFrom(srcFile, dstDir)
	if err != nil {
		fatal("导出失败: %v", err)
	}
	fmt.Printf("已导出 %d 个账号到 %s\n", n, dstDir)
	fmt.Println("请把整个目录(含 key.bin 与 state.bin)挂载进容器，例如:")
	fmt.Printf("  docker run -v %s:/data ...\n", dstDir)
}

func runImport(srcDir, dstDir string) {
	if _, err := os.Stat(filepath.Join(srcDir, "state.bin")); err != nil {
		fatal("源目录中没有 state.bin: %s", srcDir)
	}
	n, err := store.ImportInto(srcDir, dstDir)
	if err != nil {
		fatal("导入失败: %v", err)
	}
	if n == 0 {
		fmt.Println("没有需要导入的新账号(源与目标 UID 完全相同)")
		return
	}
	fmt.Printf("已从 %s 导入 %d 个账号到 %s\n", srcDir, n, dstDir)
}

func defaultExportSource(dataDir string) string {
	return filepath.Join(dataDir, "state.bin")
}

func defaultDataDir() string {
	if value := os.Getenv("WBH_DATA"); value != "" {
		return value
	}
	if value := os.Getenv("LOCALAPPDATA"); value != "" && runtime.GOOS == "windows" {
		return filepath.Join(value, "WorkBuddyHelper")
	}
	if value := os.Getenv("XDG_DATA_HOME"); value != "" {
		return filepath.Join(value, "workbuddy-helper")
	}
	if runtime.GOOS == "windows" {
		return filepath.Join("data")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "workbuddy-helper")
	}
	return "/data"
}

func defaultHost() string {
	if value := strings.TrimSpace(os.Getenv("WBH_HOST")); value != "" {
		return value
	}
	return "127.0.0.1"
}

func openBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "WorkBuddy Helper: "+format+"\n", args...)
	os.Exit(1)
}
