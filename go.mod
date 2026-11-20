module umi_upload

go 1.26.0

replace (
	golang.org/x/sys => github.com/golang/sys v0.46.0
	golang.org/x/text => github.com/golang/text v0.22.0
	golang.org/x/time => github.com/golang/time v0.10.0
)

require (
	github.com/aliyun/alibabacloud-oss-go-sdk-v2 v1.6.1
	github.com/ncruces/go-sqlite3 v0.37.0
	golang.org/x/text v0.43.0
)

require (
	github.com/ncruces/go-sqlite3-wasm/v6 v6.5.35401 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/time v0.4.0 // indirect
)
