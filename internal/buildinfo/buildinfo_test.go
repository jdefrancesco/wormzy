package buildinfo_test

import (
	"testing"

	"github.com/jdefrancesco/wormzy/internal/buildinfo"
)

func TestInfo_VersionString(t *testing.T) {
	tests := []struct {
		name string
		info buildinfo.Info
		want string
	}{
		{
			name: "release",
			info: buildinfo.Info{Version: "v0.2.0"},
			want: "v0.2.0",
		},
		{
			name: "modified development build",
			info: buildinfo.Info{Version: "dev", Modified: true},
			want: "dev-dirty",
		},
		{
			name: "dirty suffix is not duplicated",
			info: buildinfo.Info{Version: "v0.2.0-3-gabcdef-dirty", Modified: true},
			want: "v0.2.0-3-gabcdef-dirty",
		},
		{
			name: "Go dirty suffix is not duplicated",
			info: buildinfo.Info{Version: "v0.2.1-0.20260821120000-abcdef123456+dirty", Modified: true},
			want: "v0.2.1-0.20260821120000-abcdef123456+dirty",
		},
		{
			name: "empty version falls back to dev",
			info: buildinfo.Info{},
			want: "dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.VersionString(); got != tt.want {
				t.Fatalf("VersionString() = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestInfo_Format(t *testing.T) {
	info := buildinfo.Info{
		Version:   "v0.2.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: "2026-08-21T20:30:00Z",
		GoVersion: "go1.27.0",
	}

	got := info.Format("wormzy")
	want := "wormzy v0.2.0\n" +
		"commit: 0123456789abcdef0123456789abcdef01234567\n" +
		"built: 2026-08-21T20:30:00Z\n" +
		"go: go1.27.0"
	if got != want {
		t.Fatalf("Format() = %q; want %q", got, want)
	}
}

func TestInfo_FormatOmitsUnavailableMetadata(t *testing.T) {
	got := (buildinfo.Info{Version: "dev"}).Format("relay")
	if got != "relay dev" {
		t.Fatalf("Format() = %q; want %q", got, "relay dev")
	}
}
