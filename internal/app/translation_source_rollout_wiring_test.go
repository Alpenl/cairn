package app

import (
	"testing"

	"webtag/internal/config"
	"webtag/internal/observability"
)

func TestTranslationSchedulerOptionsFromConfigWiresSourceRollout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		stage      config.TranslationSourceRolloutStage
		wantStrict bool
	}{
		{name: "compat accepts legacy unverified requests", stage: config.TranslationSourceRolloutCompat, wantStrict: false},
		{name: "strict requires verified identity", stage: config.TranslationSourceRolloutStrict, wantStrict: true},
		{name: "unvalidated zero value fails closed", stage: "", wantStrict: true},
		{name: "unvalidated unknown value fails closed", stage: "unknown", wantStrict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metrics := observability.NewMetrics()
			got := translationSchedulerOptionsFromConfig(
				config.Config{TranslationSourceRollout: tc.stage},
				&persistenceLayer{metrics: metrics},
				nil,
			)
			if got.StrictSourceIdentity != tc.wantStrict {
				t.Fatalf("StrictSourceIdentity = %v, want %v for rollout %q", got.StrictSourceIdentity, tc.wantStrict, tc.stage)
			}
			if got.Metrics != metrics {
				t.Fatal("scheduler metrics do not use the composition root registry")
			}
		})
	}
}
