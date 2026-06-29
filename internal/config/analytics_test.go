package config

import "testing"

func TestResolveAnalytics(t *testing.T) {
	fileAudit := AuditConfig{Sinks: []AuditSink{{Type: "stdout"}, {Type: "file", Path: "/var/lib/inferplane/audit.jsonl"}}}
	stdoutOnly := AuditConfig{Sinks: []AuditSink{{Type: "stdout"}}}

	cases := []struct {
		name     string
		c        Config
		wantPath string
		wantOn   bool
	}{
		{"disabled", Config{Analytics: AnalyticsConfig{Disabled: true, Path: "x.db"}, Audit: fileAudit}, "", false},
		{"explicit path, no file sink", Config{Analytics: AnalyticsConfig{Path: "/data/a.db"}, Audit: stdoutOnly}, "/data/a.db", true},
		{"derive from file sink", Config{Audit: fileAudit}, "/var/lib/inferplane/analytics.db", true},
		{"no path, no file sink → off", Config{Audit: stdoutOnly}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, on := ResolveAnalytics(&tc.c)
			if p != tc.wantPath || on != tc.wantOn {
				t.Fatalf("got (%q,%v), want (%q,%v)", p, on, tc.wantPath, tc.wantOn)
			}
		})
	}
}
