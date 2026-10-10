package history

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestAlertHoldDedupCooldownResolutionAndReopen(t *testing.T) {
	store := openTestStore(t)
	bindCommandEpoch(t, store)
	ctx := t.Context()
	started := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO notification_endpoints (
		id, name, kind, config_ref, enabled, created_at, updated_at
	) VALUES ('endpoint-1', 'local', 'aitext', 'notifications.local', 1, ?, ?)`,
		timeMillis(started), timeMillis(started)); err != nil {
		t.Fatal(err)
	}
	rules, err := store.AlertRulesForEvent(ctx, "runner.planned")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ID != "runner-stuck" ||
		rules[0].Hold != 10*time.Minute {
		t.Fatalf("runner.planned rules = %+v", rules)
	}
	observation := alerts.Observation{
		Rule: rules[0], DedupKey: "session-1",
		Summary:       "Runner provisioning is stuck",
		Details:       json.RawMessage(`{"pool":"linux"}`),
		SourceEventID: "event-1", ObservedAt: started,
		CorrelationID: "corr-1",
	}
	instance, err := store.ObserveAlert(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	if instance.State != alerts.StatePending || instance.Version != 1 {
		t.Fatalf("initial alert = %+v", instance)
	}
	duplicate := observation
	duplicate.ObservedAt = started.Add(time.Minute)
	replayed, err := store.ObserveAlert(ctx, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Version != instance.Version || replayed.OccurrenceCount != 1 {
		t.Fatalf("replayed event changed alert = %+v", replayed)
	}
	assertAlertDeliveryCount(t, store, instance.ID, 0)

	promoted, err := store.PromoteDueAlerts(ctx, started.Add(599*time.Second))
	if err != nil || len(promoted) != 0 {
		t.Fatalf("early promotion = %+v err=%v", promoted, err)
	}
	promoted, err = store.PromoteDueAlerts(ctx, started.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted) != 1 || promoted[0].State != alerts.StateOpen ||
		promoted[0].Version != 2 {
		t.Fatalf("due promotion = %+v", promoted)
	}
	assertAlertDeliveryCount(t, store, instance.ID, 1)

	observation.ObservedAt = started.Add(11 * time.Minute)
	observation.SourceEventID = "event-2"
	instance, err = store.ObserveAlert(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	if instance.OccurrenceCount != 2 {
		t.Fatalf("deduplicated alert = %+v", instance)
	}
	assertAlertDeliveryCount(t, store, instance.ID, 1)

	observation.ObservedAt = started.Add(26 * time.Minute)
	observation.SourceEventID = "event-3"
	if _, err := store.ObserveAlert(ctx, observation); err != nil {
		t.Fatal(err)
	}
	assertAlertDeliveryCount(t, store, instance.ID, 2)

	if err := store.ResolveAlertByRuleKey(
		ctx, "runner-stuck", "session-1", started.Add(27*time.Minute), "corr-2",
	); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Alert(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != alerts.StateResolved || resolved.ResolvedAt == nil {
		t.Fatalf("resolved alert = %+v", resolved)
	}

	observation.ObservedAt = started.Add(30 * time.Minute)
	observation.SourceEventID = "event-4"
	reopened, err := store.ObserveAlert(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != alerts.StatePending || reopened.ResolvedAt != nil ||
		reopened.OccurrenceCount != 1 {
		t.Fatalf("reopened alert = %+v", reopened)
	}
}

func TestAlertOperatorLifecycleIsVersionedAuditedAndSilenced(t *testing.T) {
	store := openTestStore(t)
	bindCommandEpoch(t, store)
	ctx := t.Context()
	started := time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO notification_endpoints (
		id, name, kind, config_ref, enabled, created_at, updated_at
	) VALUES ('endpoint-1', 'local', 'aitext', 'notifications.local', 1, ?, ?)`,
		timeMillis(started), timeMillis(started)); err != nil {
		t.Fatal(err)
	}
	rules, err := store.AlertRulesForEvent(ctx, "runner.failed")
	if err != nil || len(rules) != 1 {
		t.Fatalf("runner.failed rules = %+v err=%v", rules, err)
	}
	instance, err := store.ObserveAlert(ctx, alerts.Observation{
		Rule: rules[0], DedupKey: "session-2", Summary: rules[0].Name,
		Details:       json.RawMessage(`{"pool":"windows"}`),
		SourceEventID: "event-failed", ObservedAt: started,
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.State != alerts.StateOpen {
		t.Fatalf("immediate alert = %+v", instance)
	}
	assertAlertDeliveryCount(t, store, instance.ID, 1)

	acknowledged, err := store.AcknowledgeAlert(ctx, alerts.AcknowledgeRequest{
		AlertID: instance.ID, IdempotencyKey: "ack-1",
		ExpectedVersion: instance.Version, ActorID: "operator-1",
		Reason: "investigating", CorrelationID: "corr-ack",
		OccurredAt: started.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.AcknowledgedBy != "operator-1" || acknowledged.Version != 2 {
		t.Fatalf("acknowledged alert = %+v", acknowledged)
	}
	replayedAck, err := store.AcknowledgeAlert(ctx, alerts.AcknowledgeRequest{
		AlertID: instance.ID, IdempotencyKey: "ack-1",
		ExpectedVersion: instance.Version, ActorID: "operator-1",
		Reason: "investigating", CorrelationID: "retry-correlation",
		OccurredAt: started.Add(90 * time.Second),
	})
	if err != nil || replayedAck.Version != acknowledged.Version {
		t.Fatalf("replayed acknowledgement = %+v err=%v", replayedAck, err)
	}
	if _, err := store.AcknowledgeAlert(ctx, alerts.AcknowledgeRequest{
		AlertID: instance.ID, IdempotencyKey: "ack-1",
		ExpectedVersion: instance.Version, ActorID: "operator-1",
		Reason: "different reason", OccurredAt: started.Add(90 * time.Second),
	}); !errors.Is(err, alerts.ErrIdempotencyConflict) {
		t.Fatalf("acknowledgement idempotency error = %v", err)
	}
	if _, err := store.AcknowledgeAlert(ctx, alerts.AcknowledgeRequest{
		AlertID: instance.ID, IdempotencyKey: "ack-stale",
		ExpectedVersion: 1, ActorID: "operator-1",
		OccurredAt: started.Add(2 * time.Minute),
	}); !errors.Is(err, alerts.ErrStateConflict) {
		t.Fatalf("stale acknowledgement error = %v", err)
	}

	silenced, silence, err := store.SilenceAlert(ctx, alerts.SilenceRequest{
		AlertID: instance.ID, IdempotencyKey: "silence-1",
		ExpectedVersion: acknowledged.Version,
		Until:           started.Add(2 * time.Hour), ActorID: "operator-1",
		Reason: "planned maintenance", CorrelationID: "corr-silence",
		OccurredAt: started.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if silenced.SilencedUntil == nil || silence.AlertID != instance.ID ||
		silenced.Version != 3 {
		t.Fatalf("silenced alert = %+v silence=%+v", silenced, silence)
	}

	annotation, err := store.AddAlertAnnotation(ctx, alerts.AnnotationRequest{
		AlertID: instance.ID, IdempotencyKey: "annotation-1",
		Body:    "Backend maintenance is in progress.",
		ActorID: "operator-1", CorrelationID: "corr-note",
		OccurredAt: started.Add(3 * time.Minute),
	})
	if err != nil || annotation.AlertID != instance.ID {
		t.Fatalf("annotation = %+v err=%v", annotation, err)
	}

	resolved, err := store.TransitionAlert(ctx, alerts.TransitionRequest{
		AlertID: instance.ID, IdempotencyKey: "resolve-1",
		ExpectedState:   alerts.StateOpen,
		ExpectedVersion: silenced.Version, To: alerts.StateResolved,
		ActorKind: operations.ActorOperator, ActorID: "operator-1",
		Reason: "maintenance completed", CorrelationID: "corr-resolve",
		OccurredAt: started.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != alerts.StateResolved || resolved.Version != 4 {
		t.Fatalf("operator resolution = %+v", resolved)
	}
	assertAlertDeliveryCount(t, store, instance.ID, 1)

	var transitions, audits, annotations, silences int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM alert_transitions WHERE alert_id=?`:  &transitions,
		`SELECT COUNT(*) FROM alert_audit_events WHERE alert_id=?`: &audits,
		`SELECT COUNT(*) FROM alert_annotations WHERE alert_id=?`:  &annotations,
		`SELECT COUNT(*) FROM alert_silences WHERE alert_id=?`:     &silences,
	} {
		if err := store.db.QueryRowContext(ctx, query, instance.ID).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if transitions != 5 || audits != 5 || annotations != 1 || silences != 1 {
		t.Fatalf("evidence transitions=%d audits=%d annotations=%d silences=%d",
			transitions, audits, annotations, silences)
	}
}

func TestNotificationDeliveryLeaseSurvivesWorkerRestart(t *testing.T) {
	store := openTestStore(t)
	bindCommandEpoch(t, store)
	ctx := t.Context()
	started := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	if err := store.SyncNotificationEndpoints(ctx, []alerts.Endpoint{{
		ID: alerts.EndpointID("notifications.aitext"), Name: "AiText",
		Kind: "aitext", ConfigRef: "notifications.aitext", Enabled: true,
	}}, started); err != nil {
		t.Fatal(err)
	}
	rules, err := store.AlertRulesForEvent(ctx, "runner.failed")
	if err != nil || len(rules) != 1 {
		t.Fatalf("runner.failed rules = %+v err=%v", rules, err)
	}
	if _, err := store.ObserveAlert(ctx, alerts.Observation{
		Rule: rules[0], DedupKey: "session-restart", Summary: rules[0].Name,
		Details: json.RawMessage(`{}`), SourceEventID: "event-restart",
		ObservedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimNotificationDelivery(ctx, "worker-1", started, time.Minute)
	if err != nil || first.Attempt != 1 {
		t.Fatalf("first claim = %+v err=%v", first, err)
	}
	if _, err := store.ClaimNotificationDelivery(
		ctx, "worker-2", started.Add(59*time.Second), time.Minute,
	); !errors.Is(err, alerts.ErrNotFound) {
		t.Fatalf("unexpired lease claim error = %v", err)
	}
	recovered, err := store.ClaimNotificationDelivery(
		ctx, "worker-2", started.Add(time.Minute), time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || recovered.Attempt != 2 ||
		recovered.LeaseOwner != "worker-2" {
		t.Fatalf("recovered delivery = %+v", recovered)
	}
	if err := store.CompleteNotificationDelivery(
		ctx, recovered.ID, "worker-2", started.Add(61*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNotificationDelivery(
		ctx, "worker-3", started.Add(2*time.Minute), time.Minute,
	); !errors.Is(err, alerts.ErrNotFound) {
		t.Fatalf("completed delivery was reclaimed: %v", err)
	}
}

func TestAlertSearchProjectionAndRetention(t *testing.T) {
	store := openTestStore(t)
	bindCommandEpoch(t, store)
	ctx := t.Context()
	old := time.Date(2025, 10, 1, 12, 0, 0, 0, time.UTC)
	current := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if err := store.SyncNotificationEndpoints(ctx, []alerts.Endpoint{{
		ID: alerts.EndpointID("notifications.aitext"), Name: "AiText",
		Kind: "aitext", ConfigRef: "notifications.aitext", Enabled: true,
	}}, old); err != nil {
		t.Fatal(err)
	}
	rules, err := store.AlertRulesForEvent(ctx, "runner.failed")
	if err != nil || len(rules) != 1 {
		t.Fatalf("runner.failed rules = %+v err=%v", rules, err)
	}
	expired, err := store.ObserveAlert(ctx, alerts.Observation{
		Rule: rules[0], DedupKey: "expired-session",
		Summary:       "Runner provisioning failed",
		Details:       json.RawMessage(`{"pool":"linux"}`),
		SourceEventID: "event-old", ObservedAt: old,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddAlertAnnotation(ctx, alerts.AnnotationRequest{
		AlertID: expired.ID, IdempotencyKey: "annotation-old",
		Body: "Backend socket was unavailable.", ActorID: "operator-1",
		OccurredAt: old.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAlert(ctx, alerts.TransitionRequest{
		AlertID: expired.ID, IdempotencyKey: "resolve-old",
		ExpectedState: alerts.StateOpen, ExpectedVersion: expired.Version,
		To: alerts.StateResolved, ActorKind: operations.ActorOperator,
		ActorID: "operator-1", Reason: "backend recovered",
		OccurredAt: old.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.ClaimNotificationDelivery(ctx, "worker", old, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteNotificationDelivery(
		ctx, delivery.ID, "worker", old.Add(3*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	active, err := store.ObserveAlert(ctx, alerts.Observation{
		Rule: rules[0], DedupKey: "active-session",
		Summary:       "Runner provisioning failed",
		Details:       json.RawMessage(`{"pool":"windows"}`),
		SourceEventID: "event-current", ObservedAt: current,
	})
	if err != nil {
		t.Fatal(err)
	}

	alertResults, err := store.Search(ctx, SearchOptions{
		Query: "provisioning", EntityType: "alert", Limit: 10,
	})
	if err != nil || len(alertResults) != 2 {
		t.Fatalf("alert search = %+v err=%v", alertResults, err)
	}
	annotationResults, err := store.Search(ctx, SearchOptions{
		Query: "socket unavailable", EntityType: "annotation", Limit: 10,
	})
	if err != nil || len(annotationResults) != 1 ||
		annotationResults[0].Route != "/alerts?alert_id="+expired.ID {
		t.Fatalf("annotation search = %+v err=%v", annotationResults, err)
	}

	result, err := store.PruneAlertHistory(ctx, current.Add(-180*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.Alerts != 1 || result.Deliveries != 1 {
		t.Fatalf("alert prune result = %+v", result)
	}
	if _, err := store.Alert(ctx, expired.ID); !errors.Is(err, alerts.ErrNotFound) {
		t.Fatalf("expired alert lookup error = %v", err)
	}
	if retained, err := store.Alert(ctx, active.ID); err != nil ||
		retained.State != alerts.StateOpen {
		t.Fatalf("active alert = %+v err=%v", retained, err)
	}
	annotationResults, err = store.Search(ctx, SearchOptions{
		Query: "socket unavailable", EntityType: "annotation", Limit: 10,
	})
	if err != nil || len(annotationResults) != 0 {
		t.Fatalf("pruned annotation search = %+v err=%v", annotationResults, err)
	}
}

func assertAlertDeliveryCount(t *testing.T, store *Store, alertID string, want int) {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM notification_deliveries WHERE alert_id=?`, alertID).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("notification deliveries = %d, want %d", count, want)
	}
}
