package schedule

import (
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/util"
)

func BenchmarkRuleWithFolderFingerprint(b *testing.B) {
	dash := uuid.NewString()
	panel := rand.Int63()
	m := &models.AlertRule{
		ID:              rand.Int63(),
		OrgID:           rand.Int63(), // Prevent OrgID=0 as this does not pass alert rule validation.
		Title:           "TEST-ALERT-" + uuid.NewString(),
		Condition:       uuid.NewString(),
		Data:            []models.AlertQuery{models.GenerateAlertQuery(), models.GenerateAlertQuery(), models.GenerateAlertQuery()},
		Updated:         time.Now(),
		IntervalSeconds: rand.Int63(),
		Version:         rand.Int63(), // Don't generate a rule ID too big for postgres
		UID:             uuid.NewString(),
		NamespaceUID:    uuid.NewString(),
		DashboardUID:    &dash,
		PanelID:         &panel,
		RuleGroup:       "TEST-GROUP-" + util.GenerateShortUID(),
		RuleGroupIndex:  rand.Intn(1500),
		NoDataState:     models.NoData,
		ExecErrState:    models.AlertingErrState,
		For:             time.Duration(rand.Int63()),
		Annotations:     models.GenerateAlertLabels(5, "test-"),
		Labels:          models.GenerateAlertLabels(10, "test-"),
		IsPaused:        false,
	}
	folder := uuid.NewString()
	b.Run("test", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ruleWithFolder{rule: m, folderTitle: folder}.Fingerprint()
		}
	})
}
