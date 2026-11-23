package umi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3CompatibleClient is used for MinIO and other S3-compatible endpoints.
// The sample Python implementation uses AWS Signature V4 for these endpoints;
// the Alibaba OSS SDK signer is not interchangeable with AWS S3 Signature V4.
type S3CompatibleClient struct {
	params     OSSParams
	client     *minio.Client
	netCB      NetWaitCallback
	waitedMu   sync.Mutex
	waitedUsed float64
}

type S3APIError struct {
	Code       string
	StatusCode int
	Message    string
}

func (e *S3APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("S3 %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("S3 %s (HTTP %d)", e.Code, e.StatusCode)
}

func normalizeS3Error(err error) error {
	if err == nil {
		return nil
	}
	var response minio.ErrorResponse
	if errors.As(err, &response) {
		return &S3APIError{
			Code:       response.Code,
			StatusCode: response.StatusCode,
			Message:    response.Message,
		}
	}
	return err
}

func NewS3CompatibleClient(p OSSParams) (*S3CompatibleClient, error) {
	if strings.TrimSpace(p.AK) == "" || strings.TrimSpace(p.SK) == "" {
		return nil, NewUploadError("请先在右上角「配置」填写并保存 Access Key ID 和 Secret")
	}
	if strings.TrimSpace(p.Bucket) == "" {
		return nil, NewUploadError("Bucket 不能为空")
	}

	rawEndpoint := strings.TrimSpace(p.Endpoint)
	if rawEndpoint == "" {
		rawEndpoint = SDKDefaults["endpoint"]
	}
	if !strings.Contains(rawEndpoint, "://") {
		rawEndpoint = "http://" + rawEndpoint
	}
	u, err := url.Parse(rawEndpoint)
	if err != nil || u.Host == "" {
		return nil, NewUploadError("S3 Endpoint 无效: %s", rawEndpoint)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, NewUploadError("S3 Endpoint 只支持 http 或 https")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, NewUploadError("S3 Endpoint 暂不支持 URL 路径前缀，请填写到主机和端口，例如 http://10.20.100.10:9000")
	}

	parallel := p.Parallel
	if parallel < 1 {
		parallel = 4
	}
	p.Parallel = parallel

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = max(64, parallel*4)
	transport.IdleConnTimeout = 90 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 90 * time.Second
	transport.ExpectContinueTimeout = time.Second

	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(p.AK, p.SK, ""),
		Secure:       u.Scheme == "https",
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		Transport:    transport,
		MaxRetries:   1, // use the application's bounded retry + network-wait policy
	})
	if err != nil {
		return nil, NewUploadError("创建 S3/MinIO 客户端失败: %v", err)
	}
	return &S3CompatibleClient{params: p, client: client}, nil
}

func (c *S3CompatibleClient) BucketName() string { return c.params.Bucket }

func (c *S3CompatibleClient) SetNetCallback(cb NetWaitCallback) { c.netCB = cb }

func (c *S3CompatibleClient) KeyOf(uri string) (string, error) {
	prefix := "oss://" + c.params.Bucket + "/"
	if !strings.HasPrefix(uri, prefix) {
		return "", NewUploadError("不是 %s 桶里的路径: %s", c.params.Bucket, uri)
	}
	return uri[len(prefix):], nil
}

func (c *S3CompatibleClient) TestBucket() error {
	ctx, cancel := context.WithTimeout(AppCtx, 8*time.Second)
	defer cancel()
	exists, err := c.client.BucketExists(ctx, c.params.Bucket)
	if err != nil {
		return normalizeS3Error(err)
	}
	if !exists {
		return NewUploadError("Bucket %s 不存在或当前 AK 无权访问", c.params.Bucket)
	}
	return nil
}

func (c *S3CompatibleClient) PutObject(uri string, data []byte) error {
	key, err := c.KeyOf(uri)
	if err != nil {
		return err
	}
	return c.runWithRetry(func() error {
		_, putErr := c.client.PutObject(AppCtx, c.params.Bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
			PartSize:             uint64(PartSize),
			NumThreads:            uint(c.params.Parallel),
			DisableContentSha256: true,
		})
		return normalizeS3Error(putErr)
	})
}

func (c *S3CompatibleClient) ReadObject(uri string) ([]byte, bool, error) {
	key, err := c.KeyOf(uri)
	if err != nil {
		return nil, false, err
	}
	var out []byte
	var found bool
	err = c.runWithRetry(func() error {
		out = nil
		found = false
		obj, getErr := c.client.GetObject(AppCtx, c.params.Bucket, key, minio.GetObjectOptions{})
		if getErr != nil {
			return normalizeS3Error(getErr)
		}
		defer obj.Close()

		if _, statErr := obj.Stat(); statErr != nil {
			if isNoSuchKey(normalizeS3Error(statErr)) {
				return nil
			}
			return normalizeS3Error(statErr)
		}
		data, readErr := io.ReadAll(obj)
		if readErr != nil {
			return normalizeS3Error(readErr)
		}
		out = data
		found = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, found, nil
}

func (c *S3CompatibleClient) ListObjects(uriPrefix string) ([]RemoteObj, error) {
	prefix, err := c.KeyOf(uriPrefix)
	if err != nil {
		return nil, err
	}
	var result []RemoteObj
	err = c.runWithRetry(func() error {
		result = nil
		ctx, cancel := context.WithCancel(AppCtx)
		defer cancel()
		for obj := range c.client.ListObjects(ctx, c.params.Bucket, minio.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
			MaxKeys:   1000,
		}) {
			if obj.Err != nil {
				return normalizeS3Error(obj.Err)
			}
			if !strings.HasPrefix(obj.Key, prefix) {
				continue
			}
			name := strings.TrimPrefix(obj.Key, prefix)
			if name == "" || strings.HasSuffix(name, "/") {
				continue
			}
			result = append(result, RemoteObj{Name: name, Size: obj.Size})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type s3ProgressReader struct {
	r       io.Reader
	onDelta func(int64)
}

func (r *s3ProgressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && r.onDelta != nil {
		r.onDelta(int64(n))
	}
	return n, err
}

func (c *S3CompatibleClient) UploadFile(localPath, key string, size int64, _ string, onDelta func(int64)) error {
	if size < 0 {
		return NewUploadError("本地文件大小无效: %s", localPath)
	}
	parallel := CurrentUploadParallel(c.params.Parallel)

	var reported int64
	var progressMu sync.Mutex
	rollbackProgress := func() {
		progressMu.Lock()
		n := reported
		reported = 0
		progressMu.Unlock()
		if n > 0 && onDelta != nil {
			onDelta(-n)
		}
	}
	return c.runWithRetry(func() error {
		rollbackProgress()
		f, err := os.Open(localPath)
		if err != nil {
			return err
		}
		defer f.Close()

		reader := &s3ProgressReader{
			r: f,
			onDelta: func(n int64) {
				progressMu.Lock()
				reported += n
				progressMu.Unlock()
				if onDelta != nil {
					onDelta(n)
				}
			},
		}

		opts := minio.PutObjectOptions{
			PartSize:             uint64(PartSize),
			NumThreads:            uint(parallel),
			DisableContentSha256: true,
		}
		uploadSize := size
		if size >= MultipartThreshold {
			// Unknown-length streaming activates MinIO Go SDK's parallel multipart
			// reader. It reads the local file sequentially and uploads parts in parallel.
			uploadSize = -1
			opts.ConcurrentStreamParts = true
		}
		_, putErr := c.client.PutObject(AppCtx, c.params.Bucket, key, reader, uploadSize, opts)
		if putErr != nil {
			rollbackProgress()
			return normalizeS3Error(putErr)
		}

		progressMu.Lock()
		remaining := size - reported
		reported = size
		progressMu.Unlock()
		if remaining > 0 && onDelta != nil {
			onDelta(remaining)
		}
		return nil
	})
}

func (c *S3CompatibleClient) waitNetworkIfAllowed(cause error) error {
	budgetSec := c.params.NetWaitMinutes * 60
	if budgetSec <= 0 || !LooksNetwork(cause) {
		return cause
	}
	c.waitedMu.Lock()
	remaining := budgetSec - c.waitedUsed
	c.waitedMu.Unlock()
	if remaining <= 0 {
		return NewUploadError("网络中断超过 %g 分钟仍未恢复，已保留上传现场，网络恢复后重新上传即可重试: %v", c.params.NetWaitMinutes, cause)
	}

	started := time.Now()
	if c.netCB != nil {
		c.netCB("down", 0, cause.Error())
	}
	interval := ProbeMin
	for {
		waited := time.Since(started).Seconds()
		if waited >= remaining {
			if c.netCB != nil {
				c.netCB("giveup", waited, cause.Error())
			}
			return NewUploadError("网络中断超过 %g 分钟仍未恢复，已保留上传现场，网络恢复后重新上传即可重试: %v", c.params.NetWaitMinutes, cause)
		}
		sleep := mathMin(interval, remaining-waited)
		if sleep > 0 {
			select {
			case <-AppCtx.Done():
				return AppCtx.Err()
			case <-time.After(time.Duration(sleep * float64(time.Second))):
			}
		}
		waited = time.Since(started).Seconds()
		if c.netCB != nil {
			c.netCB("tick", waited, "")
		}
		if err := c.TestBucket(); err == nil {
			c.waitedMu.Lock()
			c.waitedUsed += waited
			c.waitedMu.Unlock()
			if c.netCB != nil {
				c.netCB("up", waited, "")
			}
			return nil
		}
		interval = mathMin(interval*1.5, ProbeMax)
	}
}

func (c *S3CompatibleClient) runWithRetry(op func() error) error {
	tries := 0
	netEvents := 0
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
		if c.params.NetWaitMinutes > 0 && LooksNetwork(err) && netEvents < 60 {
			netEvents++
			if waitErr := c.waitNetworkIfAllowed(err); waitErr != nil {
				return waitErr
			}
			continue
		}
		tries++
		if tries >= 3 {
			return err
		}
		delay := time.Duration(tries) * 300 * time.Millisecond
		select {
		case <-AppCtx.Done():
			return AppCtx.Err()
		case <-time.After(delay):
		}
	}
}

func mathMin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

