//go:build !windows

package transport

import (
	"math"
	"testing"
)

// TestDiskFreeBytesFromStat validates filesystem values before unsigned arithmetic.
func TestDiskFreeBytesFromStat(t *testing.T) {
	tests := []struct {
		name      string
		available uint64
		blockSize int64
		want      uint64
		wantErr   bool
	}{
		{
			name:      "typical filesystem",
			available: 10,
			blockSize: 4096,
			want:      40_960,
		},
		{
			name:      "no available blocks",
			blockSize: 4096,
		},
		{
			name:      "largest safe product",
			available: math.MaxUint64 / 2,
			blockSize: 2,
			want:      math.MaxUint64 - 1,
		},
		{
			name:      "zero block size",
			available: 1,
			wantErr:   true,
		},
		{
			name:      "negative block size",
			available: 1,
			blockSize: -1,
			wantErr:   true,
		},
		{
			name:      "product overflow",
			available: math.MaxUint64,
			blockSize: 2,
			wantErr:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := diskFreeBytesFromStat(test.available, test.blockSize)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("diskFreeBytesFromStat() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("diskFreeBytesFromStat() = %d, want %d", got, test.want)
			}
		})
	}
}
