package backup

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func TestCronJobUsesBuildJobTemplate(t *testing.T) {
	cases := map[string]func(*borgbasev1.ScheduledBackup){
		"defaults": nil,
		"with a volume": func(sb *borgbasev1.ScheduledBackup) {
			sb.Spec.Volume = &borgbasev1.VolumeSpec{ExistingClaim: "app-data"}
			sb.Spec.Sources = []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeFiles}}
		},
		"with a database": func(sb *borgbasev1.ScheduledBackup) {
			sb.Spec.Database = &borgbasev1.DatabaseSpec{
				Engine: borgbasev1.DatabaseEngineMariaDB,
				Host:   "mariadb", Name: testDBName, User: "root",
			}
			sb.Spec.Sources = []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeMariaDB}}
		},
		"suspended": func(sb *borgbasev1.ScheduledBackup) { sb.Spec.Suspend = true },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			sb, repo, cfg := testBackup(mutate), testRepo(), testConfig()

			tmpl, err := BuildJobTemplate(sb, repo, cfg)
			if err != nil {
				t.Fatalf("BuildJobTemplate: %v", err)
			}
			cj, err := BuildCronJob(sb, repo, cfg)
			if err != nil {
				t.Fatalf("BuildCronJob: %v", err)
			}

			if !equality.Semantic.DeepEqual(cj.Spec.JobTemplate, tmpl) {
				t.Errorf("CronJob job template diverged from BuildJobTemplate\n cronjob: %+v\ntemplate: %+v",
					cj.Spec.JobTemplate, tmpl)
			}
		})
	}
}

func TestJobTemplateCarriesManagedByLabel(t *testing.T) {
	tmpl, err := BuildJobTemplate(testBackup(nil), testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildJobTemplate: %v", err)
	}
	if got := tmpl.Labels[labelManagedBy]; got != managedByValue {
		t.Errorf("job template label %s = %q, want %q", labelManagedBy, got, managedByValue)
	}
}

func TestBuildJobTemplateRequiresAnImage(t *testing.T) {
	cfg := testConfig()
	cfg.Image = ""
	if _, err := BuildJobTemplate(testBackup(nil), testRepo(), cfg); err == nil {
		t.Error("expected an error when no image is configured")
	}
}
