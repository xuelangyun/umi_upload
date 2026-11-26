package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"umi_upload/umi"
)

// Version is injected at build time via -ldflags "-X main.Version=..."
var Version = "dev"

//go:embed web
var embeddedWeb embed.FS

func resolveWebFS() fs.FS {
	diskWeb := filepath.Join(umi.Here, "web")
	if st, err := os.Stat(filepath.Join(diskWeb, "index.html")); err == nil && !st.IsDir() {
		return os.DirFS(diskWeb)
	}
	sub, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		panic(err)
	}
	return sub
}

func main() {
	port := flag.Int("port", 8765, "HTTP 监听端口")
	host := flag.String("host", "127.0.0.1", "HTTP 监听地址")
	dbFlag := flag.String("db", umi.DBPath, "SQLite 数据库路径")
	noBrowser := flag.Bool("no-browser", false, "启动时不自动打开浏览器")
	showVer := flag.Bool("version", false, "输出版本号并退出")
	flag.Parse()

	if *showVer {
		fmt.Println(Version)
		return
	}

	// A binary opened from a graphical file manager has no terminal attached.
	// Reopen it in the user's terminal so logs remain visible and closing that
	// terminal also ends the foreground server process.
	if maybeRelaunchInTerminal() {
		return
	}

	if *dbFlag != "" {
		umi.DBPath = *dbFlag
	}

	browserHost := *host
	if browserHost == "" || browserHost == "0.0.0.0" || browserHost == "::" {
		browserHost = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s:%d", browserHost, *port)
	listenAddr := fmt.Sprintf("%s:%d", *host, *port)

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		// If another instance is already listening on this port, open the browser to it directly.
		cli := &http.Client{Timeout: 1500 * time.Millisecond}
		if resp, herr := cli.Get(url + "/api/state?light=1"); herr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				fmt.Printf("UMI 数据中心已在运行：%s\n正在为您打开浏览器…\n", url)
				if !*noBrowser {
					umi.OpenBrowserURL(url)
				}
				return
			}
		}
		fmt.Fprintf(os.Stderr, "[错误] 无法监听 %s: %v\n", listenAddr, err)
		time.Sleep(3 * time.Second)
		os.Exit(1)
	}

	if err := umi.InitDB(); err != nil {
		fmt.Fprintf(os.Stderr, "[错误] 初始化数据库失败 (%s): %v\n", umi.DBPath, err)
		time.Sleep(3 * time.Second)
		os.Exit(1)
	}
	umi.ApplyCfg()
	umi.InstallHandlers()

	srv := &http.Server{
		Handler:           umi.NewHandler(resolveWebFS()),
		ReadHeaderTimeout: 15 * time.Second,
	}
	umi.SetShutdownCallback(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	fmt.Printf("UMI 数据中心（Go 单文件版 %s）已启动：%s\n数据库：%s\n直接关闭本窗口即可退出。\n", Version, url, umi.DBPath)
	if !*noBrowser {
		go func() {
			time.Sleep(400 * time.Millisecond)
			umi.OpenBrowserURL(url)
		}()
	}

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[错误] HTTP 服务异常退出: %v\n", err)
		os.Exit(1)
	}
}
