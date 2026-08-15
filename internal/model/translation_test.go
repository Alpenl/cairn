package model

import "testing"

func TestTranslationJobsRolloutPolicyMatrix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		stage           TranslationJobsRolloutStage
		legacyWorker    bool
		v2Worker        bool
		schedule        TranslationJobScheduleProtocol
		allowReschedule bool
	}{
		{
			stage:        TranslationJobsRolloutCompatV1,
			legacyWorker: true, v2Worker: true,
			schedule: TranslationJobScheduleLegacy,
		},
		{
			stage:        TranslationJobsRolloutDrainV1,
			legacyWorker: true, v2Worker: true,
			schedule: TranslationJobSchedulePaused,
		},
		{
			stage:           TranslationJobsRolloutStrictV2,
			v2Worker:        true,
			schedule:        TranslationJobScheduleV2,
			allowReschedule: true,
		},
	} {
		t.Run(string(tc.stage), func(t *testing.T) {
			t.Parallel()
			policy := tc.stage.JobPolicy()
			if policy.RegisterLegacyWorker != tc.legacyWorker ||
				policy.RegisterV2Worker != tc.v2Worker ||
				policy.ScheduleProtocol != tc.schedule ||
				policy.AllowExistingReschedule != tc.allowReschedule {
				t.Fatalf("JobPolicy() = %+v", policy)
			}
		})
	}
}

func TestInvalidTranslationJobsRolloutPolicyFailsClosed(t *testing.T) {
	t.Parallel()

	policy := TranslationJobsRolloutStage("unknown").JobPolicy()
	if policy.RegisterLegacyWorker || policy.RegisterV2Worker ||
		policy.ScheduleProtocol != TranslationJobSchedulePaused || policy.AllowExistingReschedule {
		t.Fatalf("invalid JobPolicy() = %+v", policy)
	}
}
