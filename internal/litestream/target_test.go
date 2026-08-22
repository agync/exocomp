package litestream

import "testing"

func TestParseTarget(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   Target
		remote bool
	}{
		{
			name:   "bare path",
			raw:    "/mnt/nas/replica",
			want:   Target{Raw: "/mnt/nas/replica", Type: ReplicaFile, Path: "/mnt/nas/replica"},
			remote: false,
		},
		{
			name:   "file url",
			raw:    "file:///srv/replica",
			want:   Target{Raw: "file:///srv/replica", Type: ReplicaFile, Path: "/srv/replica"},
			remote: false,
		},
		{
			name:   "s3 with endpoint and region",
			raw:    "s3://bucket/prefix?region=eu-west-1&endpoint=https://garage.lan",
			want:   Target{Raw: "s3://bucket/prefix?region=eu-west-1&endpoint=https://garage.lan", Type: ReplicaS3, Bucket: "bucket", Path: "prefix", Region: "eu-west-1", Endpoint: "https://garage.lan"},
			remote: true,
		},
		{
			name:   "sftp with user",
			raw:    "sftp://backup@host.lan/exocomp",
			want:   Target{Raw: "sftp://backup@host.lan/exocomp", Type: ReplicaSFTP, Host: "host.lan", User: "backup", Path: "/exocomp"},
			remote: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTarget(tc.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Errorf("target mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
			if got.Remote() != tc.remote {
				t.Errorf("Remote() = %v, want %v", got.Remote(), tc.remote)
			}
		})
	}
}

func TestParseTargetErrors(t *testing.T) {
	for _, raw := range []string{"", "   ", "https://example.com/x", "s3:///nobucket", "gcs://x/y"} {
		if _, err := ParseTarget(raw); err == nil {
			t.Errorf("ParseTarget(%q): want error, got nil", raw)
		}
	}
}
