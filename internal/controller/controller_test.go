package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
	"github.com/sina-devel/planb/internal/task"
)

func TestJobName(t *testing.T) {
	if got := jobName("planb-backup", "short"); got != "planb-backup-short" {
		t.Fatalf("got %q", got)
	}
	long := "a-very-long-schedule-name-that-goes-on-and-on-20261027030000"
	got := jobName("planb-backup", long)
	if len(got) > 63 {
		t.Fatalf("job name too long: %d", len(got))
	}
	if got == jobName("planb-backup", long+"x") {
		t.Fatal("different names must not collide")
	}
}

func TestLastSlot(t *testing.T) {
	sched, err := ParseSchedule("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	if s := lastSlot(sched, from, from.Add(time.Hour)); !s.IsZero() {
		t.Fatalf("expected no slot yet, got %v", s)
	}
	// Three missed runs collapse into the most recent one.
	now := time.Date(2026, 10, 4, 4, 0, 0, 0, time.UTC)
	want := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
	if s := lastSlot(sched, from, now); !s.Equal(want) {
		t.Fatalf("got %v want %v", s, want)
	}
	if _, err := ParseSchedule("CRON_TZ=Europe/Berlin @daily"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSchedule("not a cron"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNewBackupFromSchedule(t *testing.T) {
	s := &planbv1.Schedule{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "db"},
		Spec: planbv1.ScheduleSpec{Template: planbv1.BackupSpec{
			StorageLocation: "default",
			Postgres:        &planbv1.PostgresBackupSource{Databases: []string{"app"}},
		}},
	}
	b := NewBackupFromSchedule(s, time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC))
	if b.Name != "nightly-20261027030000" || b.Namespace != "db" || b.Labels[planbv1.LabelSchedule] != "nightly" {
		t.Fatalf("unexpected backup meta: %+v", b.ObjectMeta)
	}
	b.Spec.Postgres.Databases[0] = "changed"
	if s.Spec.Template.Postgres.Databases[0] != "app" {
		t.Fatal("template must be deep-copied")
	}
}

func TestBuildJob(t *testing.T) {
	o := &Options{AgentImage: "planb:test", JobTTLSecondsAfterFinished: 60}
	b := &planbv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "db", UID: "uid-1"}}
	b.SetGroupVersionKind(planbv1.GroupVersion.WithKind("Backup"))
	tk := &task.Task{Op: task.OpBackup, Name: "b1", BackupKey: task.BackupKey("planb", "b1")}
	env := pgCredEnv(planbv1.PostgresCredentials{Name: "pg"})
	job, err := o.buildJob(b, "planb-backup-b1", map[string]string{planbv1.LabelBackup: "b1"}, tk, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "planb:test" || c.Args[0] != "agent" {
		t.Fatalf("unexpected container: %+v", c)
	}
	if c.Env[0].Name != task.EnvTask || c.Env[1].ValueFrom.SecretKeyRef.Key != "username" {
		t.Fatalf("unexpected env: %+v", c.Env)
	}
	if job.OwnerReferences[0].Kind != "Backup" || job.Labels[planbv1.LabelManagedBy] != "planb" {
		t.Fatalf("unexpected job meta: %+v", job.ObjectMeta)
	}
	decoded, err := task.Decode(c.Env[0].Value)
	if err != nil || decoded.BackupKey != "planb/backups/b1/" {
		t.Fatalf("task roundtrip: %v %+v", err, decoded)
	}
}
