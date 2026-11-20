package umi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

type RemoteObj struct {
	Name string
	Size int64
}

type NetWaitCallback func(ev string, waited float64, errText string)

// OSSClient abstracts OSS bucket operations so production uses Alibaba Cloud OSS Go SDK v2
// and smoke tests can also exercise either mock HTTP servers or fake OSS implementations.
type OSSClient interface {
	BucketName() string
	KeyOf(uri string) (string, error)
	PutObject(uri string, data []byte) error
	ReadObject(uri string) ([]byte, bool, error)
	ListObjects(uriPrefix string) ([]RemoteObj, error)
	UploadFile(localPath, key string, size int64, ckptDir string, onDelta func(int64)) error
	TestBucket() error
	SetNetCallback(cb NetWaitCallback)
}

type OSSParams struct {
	StorageType    string // "aliyun" (default) or "s3", selected explicitly by the saved profile
	Endpoint       string
	AK             string
	SK             string
	Bucket         string
	Jobs           int
	Parallel       int
	NetWaitMinutes float64 // 0 = no wait, >0 = max minutes to wait & auto-recover on network outage
}

// ProbeMin and ProbeMax control network recovery polling intervals in seconds (configurable for fast tests).
var (
	ProbeMin = 5.0
	ProbeMax = 60.0
)

// NewOSSClientFunc selects the protocol by endpoint, matching sample/upload_umi_sdk.py:
// Alibaba Cloud OSS uses the Alibaba OSS SDK; MinIO/S3-compatible endpoints use AWS Signature V4.
var NewOSSClientFunc = func(p OSSParams) (OSSClient, error) {
	// Never infer the storage protocol from the hostname. Alibaba OSS is the safe default,
	// including when it is accessed through a custom CNAME or proxy endpoint.
	if strings.EqualFold(strings.TrimSpace(p.StorageType), "s3") {
		return NewS3CompatibleClient(p)
	}
	return NewAliyunOSSClient(p)
}

func BuildOSSParams(cfg map[string]string, bucket string) OSSParams {
	if cfg == nil {
		cfg = ActiveCfg()
	}
	jobs, _ := strconv.Atoi(cfg["jobs"])
	if jobs <= 0 {
		jobs = 16
	}
	parallel, _ := strconv.Atoi(cfg["parallel"])
	if parallel <= 0 {
		parallel = 4
	}
	netWait := 30.0
	if raw, ok := cfg["net_wait"]; ok && strings.TrimSpace(raw) != "" {
		if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && v >= 0 {
			netWait = v
		}
	}
	storageType := strings.ToLower(strings.TrimSpace(cfg["storage_type"]))
	if storageType == "" {
		storageType = "aliyun"
	}
	return OSSParams{
		StorageType:    storageType,
		Endpoint:       strings.TrimSpace(cfg["endpoint"]),
		AK:             strings.TrimSpace(cfg["ak"]),
		SK:             strings.TrimSpace(cfg["sk"]),
		Bucket:         strings.TrimSpace(bucket),
		Jobs:           jobs,
		Parallel:       parallel,
		NetWaitMinutes: netWait,
	}
}

var (
	reAliyunRegion1 = regexp.MustCompile(`(?i)(?:^|\.)oss-([a-z0-9-]+?)(?:-internal|-accelerate|-overseas)?\.aliyuncs\.com$`)
	reAliyunRegion2 = regexp.MustCompile(`(?i)^([a-z0-9-]+?)\.oss(?:-internal)?\.aliyuncs\.com$`)
)

func parseEndpointInfo(rawEndpoint string) (normalizedEndpoint, region string, isAliyun, usePathStyle bool) {
	ep := strings.TrimSpace(rawEndpoint)
	if ep == "" {
		ep = SDKDefaults["endpoint"]
	}
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return ep, "cn-hangzhou", false, false
	}
	host := strings.ToLower(u.Hostname())
	if m := reAliyunRegion1.FindStringSubmatch(host); m != nil {
		return ep, m[1], true, false
	}
	if m := reAliyunRegion2.FindStringSubmatch(host); m != nil {
		return ep, m[1], true, false
	}
	// Custom / MinIO / IP / localhost endpoints are S3-compatible and must use AWS Signature V4.
	return ep, "us-east-1", false, true
}

// AliyunOSSClient implements OSSClient on top of github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss.
type AliyunOSSClient struct {
	params     OSSParams
	client     *oss.Client
	probeCli   *oss.Client
	netCB      NetWaitCallback
	waitedMu   sync.Mutex
	waitedUsed float64
}

func NewAliyunOSSClient(p OSSParams) (*AliyunOSSClient, error) {
	if p.AK == "" || p.SK == "" {
		return nil, NewUploadError("请先在右上角「配置」填写并保存 Access Key ID 和 Secret")
	}
	if p.Bucket == "" {
		return nil, NewUploadError("Bucket 不能为空")
	}
	ep, region, isAliyun, pathStyle := parseEndpointInfo(p.Endpoint)
	cred := credentials.NewStaticCredentialsProvider(p.AK, p.SK)

	buildCfg := func(maxAttempts int, readWriteTimeout time.Duration) *oss.Config {
		cfg := oss.LoadDefaultConfig().
			WithCredentialsProvider(cred).
			WithEndpoint(ep).
			WithRegion(region).
			WithUsePathStyle(pathStyle).
			WithRetryMaxAttempts(maxAttempts).
			WithConnectTimeout(8 * time.Second).
			WithReadWriteTimeout(readWriteTimeout)
		if !isAliyun {
			// Retained for direct mock-client tests. Production endpoint selection
			// routes custom endpoints through NewS3CompatibleClient instead.
			cfg = cfg.WithSignatureVersion(oss.SignatureVersionV1)
		}
		return cfg
	}

	mainCli := oss.NewClient(buildCfg(2, 60*time.Second))
	probeCli := oss.NewClient(buildCfg(1, 8*time.Second))

	return &AliyunOSSClient{
		params:   p,
		client:   mainCli,
		probeCli: probeCli,
	}, nil
}

func (c *AliyunOSSClient) BucketName() string { return c.params.Bucket }

func (c *AliyunOSSClient) SetNetCallback(cb NetWaitCallback) { c.netCB = cb }

func (c *AliyunOSSClient) KeyOf(uri string) (string, error) {
	pre := "oss://" + c.params.Bucket + "/"
	if !strings.HasPrefix(uri, pre) {
		return "", NewUploadError("不是 %s 桶里的路径: %s", c.params.Bucket, uri)
	}
	return uri[len(pre):], nil
}

func (c *AliyunOSSClient) TestBucket() error {
	ctx, cancel := context.WithTimeout(AppCtx, 8*time.Second)
	defer cancel()
	_, err := c.probeCli.ListObjectsV2(ctx, &oss.ListObjectsV2Request{
		Bucket:  oss.Ptr(c.params.Bucket),
		MaxKeys: 1,
	})
	return err
}

func isNoSuchKey(err error) bool {
	if err == nil {
		return false
	}
	var svcErr *oss.ServiceError
	if errors.As(err, &svcErr) {
		if svcErr.StatusCode == http.StatusNotFound || svcErr.Code == "NoSuchKey" {
			return true
		}
	}
	var s3Err *S3APIError
	if errors.As(err, &s3Err) && (s3Err.StatusCode == http.StatusNotFound || s3Err.Code == "NoSuchKey") {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "NoSuchKey") || strings.Contains(msg, "StatusCode: 404") || strings.Contains(msg, "status code 404")
}

// LooksNetwork detects transient network/socket/DNS/5xx errors eligible for disconnect wait-and-resume.
func LooksNetwork(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) && AppCtx.Err() != nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var svcErr *oss.ServiceError
	if errors.As(err, &svcErr) {
		return svcErr.StatusCode == 408 || svcErr.StatusCode == 429 || svcErr.StatusCode >= 500
	}
	var s3Err *S3APIError
	if errors.As(err, &s3Err) {
		return s3Err.StatusCode == 408 || s3Err.StatusCode == 429 || s3Err.StatusCode >= 500
	}
	s := strings.ToLower(err.Error())
	for _, kw := range []string{
		"timeout", "timed out", "connection reset", "connection refused",
		"connection aborted", "broken pipe", "eof", "no such host",
		"temporary failure", "network is unreachable", "no route to host",
		"tls handshake", "server closed idle connection", "dial tcp",
		"read tcp", "write tcp", "http2: client connection lost",
		"serviceunavailable", "internalerror", "requesttimeout",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func (c *AliyunOSSClient) waitNetworkIfAllowed(cause error) error {
	budgetSec := c.params.NetWaitMinutes * 60.0
	if budgetSec <= 0 || !LooksNetwork(cause) {
		return cause
	}
	c.waitedMu.Lock()
	rem := budgetSec - c.waitedUsed
	c.waitedMu.Unlock()
	if rem <= 0 {
		return NewUploadError("网络中断超过 %g 分钟仍未恢复，已保留断点，网络恢复后重新上传即可续传: %v", c.params.NetWaitMinutes, cause)
	}

	t0 := time.Now()
	if c.netCB != nil {
		c.netCB("down", 0, cause.Error())
	}
	sleepSec := ProbeMin
	for {
		waited := time.Since(t0).Seconds()
		if waited >= rem {
			if c.netCB != nil {
				c.netCB("giveup", waited, cause.Error())
			}
			return NewUploadError("网络中断超过 %g 分钟仍未恢复，已保留断点，网络恢复后重新上传即可续传: %v", c.params.NetWaitMinutes, cause)
		}
		waitDur := math.Min(sleepSec, rem-waited)
		if waitDur > 0 {
			select {
			case <-AppCtx.Done():
				return AppCtx.Err()
			case <-time.After(time.Duration(waitDur * float64(time.Second))):
			}
		}
		waited = time.Since(t0).Seconds()
		if c.netCB != nil {
			c.netCB("tick", waited, "")
		}
		if probeErr := c.TestBucket(); probeErr == nil {
			c.waitedMu.Lock()
			c.waitedUsed += waited
			c.waitedMu.Unlock()
			if c.netCB != nil {
				c.netCB("up", waited, "")
			}
			return nil
		}
		sleepSec = math.Min(sleepSec*1.5, ProbeMax)
	}
}

func (c *AliyunOSSClient) runWithRetry(op func() error) error {
	tries := 0
	nets := 0
	budgetSec := c.params.NetWaitMinutes * 60.0
	for {
		if AppCtx.Err() != nil {
			return AppCtx.Err()
		}
		err := op()
		if err == nil {
			return nil
		}
		if isNoSuchKey(err) {
			return err
		}
		if budgetSec > 0 && LooksNetwork(err) && nets < 60 {
			nets++
			if werr := c.waitNetworkIfAllowed(err); werr != nil {
				return werr
			}
			continue
		}
		tries++
		if tries >= 3 {
			return err
		}
		backoff := time.Duration(tries) * 300 * time.Millisecond
		select {
		case <-AppCtx.Done():
			return AppCtx.Err()
		case <-time.After(backoff):
		}
	}
}

func (c *AliyunOSSClient) PutObject(uri string, data []byte) error {
	key, err := c.KeyOf(uri)
	if err != nil {
		return err
	}
	return c.runWithRetry(func() error {
		_, err := c.client.PutObject(AppCtx, &oss.PutObjectRequest{
			Bucket: oss.Ptr(c.params.Bucket),
			Key:    oss.Ptr(key),
			Body:   bytes.NewReader(data),
		})
		return err
	})
}

func (c *AliyunOSSClient) ReadObject(uri string) ([]byte, bool, error) {
	key, err := c.KeyOf(uri)
	if err != nil {
		return nil, false, err
	}
	var out []byte
	var found bool
	err = c.runWithRetry(func() error {
		res, gerr := c.client.GetObject(AppCtx, &oss.GetObjectRequest{
			Bucket: oss.Ptr(c.params.Bucket),
			Key:    oss.Ptr(key),
		})
		if gerr != nil {
			if isNoSuchKey(gerr) {
				found = false
				out = nil
				return nil
			}
			return gerr
		}
		defer res.Body.Close()
		b, rerr := io.ReadAll(res.Body)
		if rerr != nil {
			return rerr
		}
		out = b
		found = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, found, nil
}

func (c *AliyunOSSClient) ListObjects(uriPrefix string) ([]RemoteObj, error) {
	prefix, err := c.KeyOf(uriPrefix)
	if err != nil {
		return nil, err
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var result []RemoteObj
	err = c.runWithRetry(func() error {
		result = nil
		var token *string
		for {
			res, lerr := c.client.ListObjectsV2(AppCtx, &oss.ListObjectsV2Request{
				Bucket:            oss.Ptr(c.params.Bucket),
				Prefix:            oss.Ptr(prefix),
				MaxKeys:           1000,
				ContinuationToken: token,
			})
			if lerr != nil {
				return lerr
			}
			for _, obj := range res.Contents {
				k := oss.ToString(obj.Key)
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				rel := k[len(prefix):]
				if rel == "" || strings.HasSuffix(rel, "/") {
					continue
				}
				result = append(result, RemoteObj{
					Name: rel,
					Size: obj.Size,
				})
			}
			if !res.IsTruncated || res.NextContinuationToken == nil || *res.NextContinuationToken == "" {
				break
			}
			token = res.NextContinuationToken
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *AliyunOSSClient) UploadFile(localPath, key string, size int64, ckptDir string, onDelta func(int64)) error {
	if size < 0 {
		return NewUploadError("本地文件大小无效: %s", localPath)
	}

	parallel := c.params.Parallel
	if parallel < 1 {
		parallel = 4
	}

	// All local files go through the SDK v2 file-path API. Uploader.UploadFile
	// internally uses PutObject for files smaller than PartSize and multipart
	// for larger files. Enable persistent checkpoints for multipart-sized files.
	useCheckpoint := size >= MultipartThreshold && ckptDir != ""
	if useCheckpoint {
		if err := os.MkdirAll(ckptDir, 0o700); err != nil {
			return fmt.Errorf("创建 OSS checkpoint 目录失败: %w", err)
		}
	}

	var reported int64
	var progMu sync.Mutex
	rollbackProgress := func() {
		progMu.Lock()
		n := reported
		reported = 0
		progMu.Unlock()
		if n > 0 && onDelta != nil {
			onDelta(-n)
		}
	}

	err := c.runWithRetry(func() error {
		// A retry restarts progress accounting. For multipart uploads the SDK
		// may resume from checkpoint; the completed bytes will be reported again.
		rollbackProgress()

		uploader := oss.NewUploader(c.client, func(uo *oss.UploaderOptions) {
			uo.PartSize = PartSize
			uo.ParallelNum = parallel
			uo.LeavePartsOnError = true
			if useCheckpoint {
				uo.EnableCheckpoint = true
				uo.CheckpointDir = ckptDir
			}
		})

		req := &oss.PutObjectRequest{
			Bucket: oss.Ptr(c.params.Bucket),
			Key:    oss.Ptr(key),
			ProgressFn: func(inc, transferred, total int64) {
				if inc <= 0 || onDelta == nil {
					return
				}
				progMu.Lock()
				reported += inc
				progMu.Unlock()
				onDelta(inc)
			},
		}

		// Use Uploader.UploadFile (not UploadFrom): the SDK opens the local
		// path itself and supports persistent multipart checkpoint recovery.
		_, uploadErr := uploader.UploadFile(AppCtx, req, localPath)
		return uploadErr
	})
	if err != nil {
		rollbackProgress()
		return err
	}

	// SDK v1.6.1 does not emit ProgressFn for its internal single-part path.
	// Account for small files only after the single PutObject has completed.
	progMu.Lock()
	diff := size - reported
	reported = size
	progMu.Unlock()
	if diff > 0 && onDelta != nil {
		onDelta(diff)
	}
	return nil
}

func FormatOSSURI(bucket, key string) string {
	return fmt.Sprintf("oss://%s/%s", bucket, strings.TrimLeft(key, "/"))
}
