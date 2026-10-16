package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in      string
		want    ByteSize
		wantErr bool
	}{
		{"100B", 100, false},
		{"512KB", 524288, false},
		{"512kb", 524288, false},
		{"10MB", 10 << 20, false},
		{"10 MB", 10 << 20, false},
		{"1GB", 1 << 30, false},
		{"1Gb", 1 << 30, false},
		{"2048", 2048, false},
		{"0", 0, false}, // parseable; rejected as non-positive by ValidateLogging
		{"", 0, true},
		{"MB", 0, true},
		{"-5MB", 0, true},
		{"1.5MB", 0, true},
		{"10TB", 0, true},
		{"10XB", 0, true},
		{"1e3", 0, true},
		{"99999999999GB", 0, true},
		{"99999999999999999999", 0, true},
	}

	for _, tc := range cases {
		got, err := ParseByteSize(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseByteSize(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)

			continue
		}

		if !tc.wantErr && got != tc.want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestByteSizeUnmarshalYAML(t *testing.T) {
	var v struct {
		A ByteSize `yaml:"a"`
		B ByteSize `yaml:"b"`
	}

	if err := yaml.Unmarshal([]byte("a: 512KB\nb: 4096\n"), &v); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if v.A != 524288 || v.B != 4096 {
		t.Errorf("got A=%d B=%d, want 524288 4096", v.A, v.B)
	}

	if err := yaml.Unmarshal([]byte("a: 1.5MB\n"), &v); err == nil {
		t.Error("expected error for fractional size")
	}
}

func TestByteSizeString(t *testing.T) {
	cases := map[ByteSize]string{10 << 20: "10MB", 1536 << 10: "1536KB", 100: "100B", 0: "0B", 1 << 30: "1GB"}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(in), got, want)
		}
	}
}

func loadLoggingYAML(t *testing.T, block string) *Config {
	t.Helper()

	var cfg Config
	if err := yaml.Unmarshal([]byte(block), &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	cfg.applyDefaults()

	return &cfg
}

func TestLogRotationDefaults(t *testing.T) {
	cfg := loadLoggingYAML(t, "logging:\n  file_path: .ready/logs/ready.log\n")
	r := cfg.Logging.FileRotation

	if r.MaxSegmentSize == nil || *r.MaxSegmentSize != 10<<20 {
		t.Errorf("max_segment_size = %v, want 10MB", r.MaxSegmentSize)
	}

	if r.MaxTotalSize == nil || *r.MaxTotalSize != 100<<20 {
		t.Errorf("max_total_size = %v, want 100MB", r.MaxTotalSize)
	}

	if r.RetentionDays == nil || *r.RetentionDays != 7 {
		t.Errorf("retention_days = %v, want 7", r.RetentionDays)
	}

	warns, err := cfg.ValidateLogging()
	if err != nil || len(warns) != 0 {
		t.Errorf("ValidateLogging() = %v, %v; want no warnings, nil", warns, err)
	}
}

func TestLogRotationDefaultsSkippedWithoutFilePath(t *testing.T) {
	cfg := loadLoggingYAML(t, "logging:\n  level: info\n")
	if cfg.Logging.FileRotation.MaxSegmentSize != nil {
		t.Error("defaults applied although file_path is empty")
	}
}

func TestValidateLogging(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		errSubstr []string
		warnSub   string
	}{
		{
			name:      "zero retention",
			yaml:      "logging:\n  file_path: x.log\n  file_rotation:\n    retention_days: 0\n",
			errSubstr: []string{"logging.file_rotation.retention_days"},
		},
		{
			name:      "negative retention",
			yaml:      "logging:\n  file_path: x.log\n  file_rotation:\n    retention_days: -1\n",
			errSubstr: []string{"logging.file_rotation.retention_days"},
		},
		{
			name:      "zero segment",
			yaml:      "logging:\n  file_path: x.log\n  file_rotation:\n    max_segment_size: 0\n",
			errSubstr: []string{"logging.file_rotation.max_segment_size"},
		},
		{
			name:      "zero total",
			yaml:      "logging:\n  file_path: x.log\n  file_rotation:\n    max_total_size: 0MB\n",
			errSubstr: []string{"logging.file_rotation.max_total_size"},
		},
		{
			name:      "inconsistent cap",
			yaml:      "logging:\n  file_path: x.log\n  file_rotation:\n    max_segment_size: 50MB\n    max_total_size: 20MB\n",
			errSubstr: []string{"logging.file_rotation.max_total_size", "logging.file_rotation.max_segment_size"},
		},
		{
			name:    "cap below two segments warns",
			yaml:    "logging:\n  file_path: x.log\n  file_rotation:\n    max_segment_size: 10MB\n    max_total_size: 15MB\n",
			warnSub: "less than twice",
		},
		{
			name:    "invalid console level warns",
			yaml:    "logging:\n  console_level: verbose\n",
			warnSub: "logging.console_level",
		},
		{
			name: "console off accepted (unquoted YAML off stays a string)",
			yaml: "logging:\n  console_level: off\n  file_path: x.log\n",
		},
		{
			name:    "console off without file warns",
			yaml:    "logging:\n  console_level: \"off\"\n",
			warnSub: "only fatal records",
		},
		{
			name:    "invalid level warns",
			yaml:    "logging:\n  level: verbose\n",
			warnSub: "logging.level",
		},
		{
			name:      "invalid format is fatal",
			yaml:      "logging:\n  format: xml\n",
			errSubstr: []string{"logging.format"},
		},
		{
			name: "rotation ignored without file_path",
			yaml: "logging:\n  file_rotation:\n    retention_days: 0\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadLoggingYAML(t, tc.yaml)
			warns, err := cfg.ValidateLogging()

			if len(tc.errSubstr) > 0 {
				if err == nil {
					t.Fatalf("expected error, got nil (warnings %v)", warns)
				}

				for _, sub := range tc.errSubstr {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q does not name %q", err, sub)
					}
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.warnSub == "" {
				if len(warns) != 0 {
					t.Errorf("unexpected warnings: %v", warns)
				}

				return
			}

			if len(warns) != 1 || !strings.Contains(warns[0], tc.warnSub) {
				t.Errorf("warnings = %v, want one containing %q", warns, tc.warnSub)
			}
		})
	}
}
