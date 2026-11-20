//go:build ignore

// package_release.go builds umi_upload.exe (Windows amd64 single-file binary),
// creates umi_upload-windows-amd64.zip + SHA256SUMS.txt using only the Go standard library,
// and (when -gitlab-publish is set) uploads assets to GitLab Generic Package Registry
// and creates/updates the GitLab Release via REST API.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	outDir := flag.String("out", "dist", "Output directory for packaged artifacts")
	version := flag.String("version", "", "Release version / tag name (auto-detected from CI env if empty)")
	skipBuild := flag.Bool("skip-build", false, "Skip running go build if dist/umi_upload.exe already exists")
	gitlabPublish := flag.Bool("gitlab-publish", false, "Upload package & create/update Release via GitLab API")
	flag.Parse()

	ver := strings.TrimSpace(*version)
	if ver == "" {
		ver = resolveVersionFromEnv()
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", *outDir, err)
	}

	exePath := filepath.Join(*outDir, "umi_upload.exe")
	if !*skipBuild {
		if err := buildWindowsExe(exePath, ver); err != nil {
			fatalf("build windows exe: %v", err)
		}
	}
	if st, err := os.Stat(exePath); err != nil || st.Size() == 0 {
		fatalf("missing or empty %s", exePath)
	}

	zipName := "umi_upload-windows-amd64.zip"
	zipPath := filepath.Join(*outDir, zipName)
	bundleFiles := []struct {
		src  string
		name string
	}{
		{exePath, "umi_upload.exe"},
		{"start.bat", "start.bat"},
		{"README.md", "README.md"},
	}
	if err := createZip(zipPath, bundleFiles); err != nil {
		fatalf("create zip: %v", err)
	}

	exeSHA, err := fileSHA256(exePath)
	if err != nil {
		fatalf("sha256 exe: %v", err)
	}
	zipSHA, err := fileSHA256(zipPath)
	if err != nil {
		fatalf("sha256 zip: %v", err)
	}

	sumsPath := filepath.Join(*outDir, "SHA256SUMS.txt")
	sumsContent := fmt.Sprintf("%s  umi_upload.exe\n%s  %s\n", exeSHA, zipSHA, zipName)
	if err := os.WriteFile(sumsPath, []byte(sumsContent), 0o644); err != nil {
		fatalf("write SHA256SUMS.txt: %v", err)
	}

	fmt.Printf("[OK] Packaged version %s in %s/\n", ver, *outDir)
	fmt.Printf("  - umi_upload.exe (%s)\n", exeSHA)
	fmt.Printf("  - %s (%s)\n", zipName, zipSHA)

	if *gitlabPublish {
		if err := publishToGitLab(ver, exePath, zipPath, sumsPath, exeSHA, zipSHA); err != nil {
			fatalf("gitlab publish: %v", err)
		}
	}
}

func resolveVersionFromEnv() string {
	if tag := strings.TrimSpace(os.Getenv("CI_COMMIT_TAG")); tag != "" {
		return tag
	}
	if ghRefName := strings.TrimSpace(os.Getenv("GITHUB_REF_NAME")); os.Getenv("GITHUB_REF_TYPE") == "tag" && ghRefName != "" {
		return ghRefName
	}
	shortSHA := strings.TrimSpace(os.Getenv("CI_COMMIT_SHORT_SHA"))
	if shortSHA == "" {
		if sha := strings.TrimSpace(os.Getenv("GITHUB_SHA")); len(sha) >= 8 {
			shortSHA = sha[:8]
		}
	}
	if shortSHA == "" {
		if out, err := exec.Command("git", "rev-parse", "--short=8", "HEAD").Output(); err == nil {
			shortSHA = strings.TrimSpace(string(out))
		}
	}
	datePart := time.Now().UTC().Format("20060102")
	if shortSHA != "" {
		return fmt.Sprintf("release-%s-%s", datePart, shortSHA)
	}
	return "release-" + datePart
}

func buildWindowsExe(outPath, ver string) error {
	fmt.Printf("[BUILD] Cross-compiling %s (version=%s)...\n", outPath, ver)
	ldflags := fmt.Sprintf("-s -w -X main.Version=%s", ver)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", outPath, ".")
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=windows",
		"GOARCH=amd64",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func createZip(zipPath string, files []struct{ src, name string }) error {
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, item := range files {
		st, err := os.Stat(item.src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		hdr, err := zip.FileInfoHeader(st)
		if err != nil {
			return err
		}
		hdr.Name = item.name
		hdr.Method = zip.Deflate
		hdr.Modified = fixedTime
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		in, err := os.Open(item.src)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, in)
		_ = in.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return zw.Close()
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

func setGitLabAuth(req *http.Request) error {
	if tok := strings.TrimSpace(os.Getenv("GITLAB_TOKEN")); tok != "" {
		req.Header.Set("PRIVATE-TOKEN", tok)
		return nil
	}
	if jobTok := strings.TrimSpace(os.Getenv("CI_JOB_TOKEN")); jobTok != "" {
		req.Header.Set("JOB-TOKEN", jobTok)
		return nil
	}
	return fmt.Errorf("neither GITLAB_TOKEN nor CI_JOB_TOKEN is set")
}

func uploadGenericPackage(client *http.Client, apiBase, projID, pkgVer, filePath, remoteName string) (string, error) {
	targetURL := fmt.Sprintf("%s/projects/%s/packages/generic/umi-upload/%s/%s",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(projID),
		url.PathEscape(pkgVer),
		url.PathEscape(remoteName),
	)
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPut, targetURL, f)
	if err != nil {
		return "", err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	if err := setGitLabAuth(req); err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("PUT %s -> HTTP %d: %s", targetURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return targetURL, nil
}

func publishToGitLab(ver, exePath, zipPath, sumsPath, exeSHA, zipSHA string) error {
	apiBase := strings.TrimSpace(os.Getenv("CI_API_V4_URL"))
	projID := strings.TrimSpace(os.Getenv("CI_PROJECT_ID"))
	if apiBase == "" || projID == "" {
		return fmt.Errorf("CI_API_V4_URL and CI_PROJECT_ID are required for -gitlab-publish")
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	pkgVer := strings.TrimLeft(ver, "v")
	if pkgVer == "" {
		pkgVer = "0.0.0"
	}
	// Sanitize package version to characters allowed by GitLab Generic Packages ([0-9a-zA-Z._-]+)
	var cleanVer strings.Builder
	for _, r := range pkgVer {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '.' || r == '_' || r == '-' {
			cleanVer.WriteRune(r)
		} else {
			cleanVer.WriteByte('-')
		}
	}
	pkgVer = cleanVer.String()

	fmt.Printf("[GITLAB] Uploading assets to Generic Package Registry (umi-upload/%s)...\n", pkgVer)
	exeURL, err := uploadGenericPackage(client, apiBase, projID, pkgVer, exePath, "umi_upload.exe")
	if err != nil {
		return err
	}
	zipURL, err := uploadGenericPackage(client, apiBase, projID, pkgVer, zipPath, "umi_upload-windows-amd64.zip")
	if err != nil {
		return err
	}
	sumsURL, err := uploadGenericPackage(client, apiBase, projID, pkgVer, sumsPath, "SHA256SUMS.txt")
	if err != nil {
		return err
	}

	desc := strings.Join([]string{
		fmt.Sprintf("## UMI 数据中心 (%s)", ver),
		"",
		"- **Windows 64 位单文件免安装版**：内置 Web 前端静态资源与阿里云官方 OSS Go SDK（`alibabacloud-oss-go-sdk-v2`），无需安装 Python 或 `ossutil.exe`，下载 `umi_upload.exe` 双击即可直接启动。",
		"",
		"### SHA-256 校验值",
		"```text",
		fmt.Sprintf("%s  umi_upload.exe", exeSHA),
		fmt.Sprintf("%s  umi_upload-windows-amd64.zip", zipSHA),
		"```",
	}, "\n")

	type assetLink struct {
		Name            string `json:"name"`
		URL             string `json:"url"`
		LinkType        string `json:"link_type,omitempty"`
		DirectAssetPath string `json:"direct_asset_path,omitempty"`
	}
	links := []assetLink{
		{
			Name:            "umi_upload.exe (Windows x64 单文件直启版)",
			URL:             exeURL,
			LinkType:        "package",
			DirectAssetPath: "/umi_upload.exe",
		},
		{
			Name:            "umi_upload-windows-amd64.zip (含 start.bat 与说明文档)",
			URL:             zipURL,
			LinkType:        "package",
			DirectAssetPath: "/umi_upload-windows-amd64.zip",
		},
		{
			Name:            "SHA256SUMS.txt",
			URL:             sumsURL,
			LinkType:        "other",
			DirectAssetPath: "/SHA256SUMS.txt",
		},
	}

	payload := map[string]any{
		"name":        "UMI 数据中心 " + ver,
		"tag_name":    ver,
		"description": desc,
		"assets": map[string]any{
			"links": links,
		},
	}
	// If running on a branch commit (not an existing tag), pass ref so GitLab creates the tag + release atomically.
	if strings.TrimSpace(os.Getenv("CI_COMMIT_TAG")) == "" {
		if sha := strings.TrimSpace(os.Getenv("CI_COMMIT_SHA")); sha != "" {
			payload["ref"] = sha
		}
	}

	rawBody, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	releasesURL := fmt.Sprintf("%s/projects/%s/releases", strings.TrimRight(apiBase, "/"), url.PathEscape(projID))
	req, err := http.NewRequest(http.MethodPost, releasesURL, bytes.NewReader(rawBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := setGitLabAuth(req); err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Release for this tag already exists (e.g., retried CI job) -> update idempotently via PUT
		updatePayload := map[string]any{
			"name":        "UMI 数据中心 " + ver,
			"description": desc,
		}
		upBytes, _ := json.Marshal(updatePayload)
		putURL := fmt.Sprintf("%s/%s", releasesURL, url.PathEscape(ver))
		putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(upBytes))
		if err != nil {
			return err
		}
		putReq.Header.Set("Content-Type", "application/json")
		if err := setGitLabAuth(putReq); err != nil {
			return err
		}
		putResp, err := client.Do(putReq)
		if err != nil {
			return err
		}
		putRespBody, _ := io.ReadAll(putResp.Body)
		_ = putResp.Body.Close()
		if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
			return fmt.Errorf("PUT %s -> HTTP %d: %s", putURL, putResp.StatusCode, strings.TrimSpace(string(putRespBody)))
		}
		fmt.Printf("[GITLAB] Updated existing GitLab Release %s\n", ver)
		return nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s -> HTTP %d: %s", releasesURL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	fmt.Printf("[GITLAB] Created GitLab Release %s\n", ver)
	return nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[ERROR] "+format+"\n", args...)
	os.Exit(1)
}
