package umi

import (
	"testing"

	oss "github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
)

func TestConsecutiveOSSPartOffset(t *testing.T) {
	tests := []struct {
		name     string
		start    int32
		parts    []oss.Part
		partSize int64
		wantNext int32
		wantOK   bool
	}{
		{
			name:     "contiguous full parts",
			start:    1,
			parts:    []oss.Part{{PartNumber: 1, Size: 8}, {PartNumber: 2, Size: 8}},
			partSize: 8,
			wantNext: 3,
			wantOK:   true,
		},
		{
			name:     "gap stops resume offset",
			start:    1,
			parts:    []oss.Part{{PartNumber: 1, Size: 8}, {PartNumber: 3, Size: 8}},
			partSize: 8,
			wantNext: 2,
			wantOK:   false,
		},
		{
			name:     "short part stops resume offset",
			start:    1,
			parts:    []oss.Part{{PartNumber: 1, Size: 7}, {PartNumber: 2, Size: 8}},
			partSize: 8,
			wantNext: 1,
			wantOK:   false,
		},
		{
			name:     "unexpected part order stops resume offset",
			start:    2,
			parts:    []oss.Part{{PartNumber: 3, Size: 8}},
			partSize: 8,
			wantNext: 2,
			wantOK:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, ok := consecutiveOSSPartOffset(tt.start, tt.parts, tt.partSize)
			if next != tt.wantNext || ok != tt.wantOK {
				t.Fatalf("consecutiveOSSPartOffset() = (%d, %v), want (%d, %v)", next, ok, tt.wantNext, tt.wantOK)
			}
		})
	}
}

func TestConsecutiveOSSPartOffsetAcrossPages(t *testing.T) {
	next, ok := consecutiveOSSPartOffset(1, []oss.Part{{PartNumber: 1, Size: 8}}, 8)
	if !ok {
		t.Fatal("first page should be contiguous")
	}
	next, ok = consecutiveOSSPartOffset(next, []oss.Part{{PartNumber: 2, Size: 8}}, 8)
	if !ok || next != 3 {
		t.Fatalf("second page offset = (%d, %v), want (3, true)", next, ok)
	}
	if got := int64(next-1) * 8; got != 16 {
		t.Fatalf("resume offset = %d, want 16", got)
	}
}
