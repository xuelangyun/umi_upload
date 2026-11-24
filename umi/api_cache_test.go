package umi

import "testing"

func TestETagMatches(t *testing.T) {
	const etag = "\"dataset-v1\""
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "exact", header: etag, want: true},
		{name: "weak validator", header: "W/" + etag, want: true},
		{name: "multiple validators", header: "\"other\", " + etag, want: true},
		{name: "wildcard", header: "*", want: true},
		{name: "different", header: "\"dataset-v2\"", want: false},
		{name: "empty", header: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := etagMatches(tt.header, etag); got != tt.want {
				t.Fatalf("etagMatches(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestDatasetRowsConfigKeyTracksBucketsOnly(t *testing.T) {
	base := map[string]string{"bucket": "upload-a", "bucket_umi": "upload-b", "endpoint": "https://oss.example"}
	if got, want := datasetRowsConfigKey(base), "upload-a\x00upload-b"; got != want {
		t.Fatalf("config key = %q, want %q", got, want)
	}

	changedEndpoint := map[string]string{"bucket": "upload-a", "bucket_umi": "upload-b", "endpoint": "https://other.example"}
	if datasetRowsConfigKey(changedEndpoint) != datasetRowsConfigKey(base) {
		t.Fatal("endpoint-only changes should not invalidate dataset rows")
	}
	changedBucket := map[string]string{"bucket": "upload-c", "bucket_umi": "upload-b"}
	if datasetRowsConfigKey(changedBucket) == datasetRowsConfigKey(base) {
		t.Fatal("bucket changes must invalidate dataset rows")
	}
	changedUMIBucket := map[string]string{"bucket": "upload-a", "bucket_umi": "upload-d"}
	if datasetRowsConfigKey(changedUMIBucket) == datasetRowsConfigKey(base) {
		t.Fatal("UMI bucket changes must invalidate dataset rows")
	}
}
