package task

import "testing"

func TestBackupKey(t *testing.T) {
	cases := map[string]string{
		"":        "backups/b1/",
		"planb":   "planb/backups/b1/",
		"/planb/": "planb/backups/b1/",
		"a/b/":    "a/b/backups/b1/",
	}
	for prefix, want := range cases {
		if got := BackupKey(prefix, "b1"); got != want {
			t.Errorf("BackupKey(%q) = %q, want %q", prefix, got, want)
		}
	}
	if got := BackupsKey("planb"); got != "planb/backups/" {
		t.Errorf("BackupsKey = %q", got)
	}
}
