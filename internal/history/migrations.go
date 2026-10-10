package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{version: 1, sql: `
CREATE TABLE runner_sessions (
	id              TEXT PRIMARY KEY,
	runner_name     TEXT NOT NULL,
	pool_name       TEXT NOT NULL,
	repository      TEXT NOT NULL,
	workflow_run_id INTEGER NOT NULL DEFAULT 0,
	workflow_job_id INTEGER NOT NULL DEFAULT 0,
	status          TEXT NOT NULL,
	conclusion      TEXT NOT NULL DEFAULT '',
	started_at      INTEGER NOT NULL,
	completed_at    INTEGER,
	updated_at      INTEGER NOT NULL,
	error           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_runner_sessions_repository_started ON runner_sessions(repository, started_at DESC);
CREATE INDEX idx_runner_sessions_pending ON runner_sessions(completed_at, started_at);
CREATE INDEX idx_runner_sessions_job ON runner_sessions(repository, workflow_job_id);

CREATE TABLE workflow_runs (
	repository    TEXT NOT NULL,
	id            INTEGER NOT NULL,
	name          TEXT NOT NULL DEFAULT '',
	workflow_name TEXT NOT NULL DEFAULT '',
	event         TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL DEFAULT '',
	conclusion    TEXT NOT NULL DEFAULT '',
	head_branch   TEXT NOT NULL DEFAULT '',
	head_sha      TEXT NOT NULL DEFAULT '',
	actor         TEXT NOT NULL DEFAULT '',
	html_url      TEXT NOT NULL DEFAULT '',
	run_number    INTEGER NOT NULL DEFAULT 0,
	run_attempt   INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	started_at    INTEGER,
	completed_at  INTEGER,
	PRIMARY KEY (repository, id)
);
CREATE INDEX idx_workflow_runs_updated ON workflow_runs(repository, updated_at DESC);

CREATE TABLE workflow_jobs (
	repository   TEXT NOT NULL,
	id           INTEGER NOT NULL,
	run_id       INTEGER NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT '',
	conclusion   TEXT NOT NULL DEFAULT '',
	runner_name  TEXT NOT NULL DEFAULT '',
	runner_group TEXT NOT NULL DEFAULT '',
	html_url     TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	started_at   INTEGER,
	completed_at INTEGER,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (repository, id),
	FOREIGN KEY (repository, run_id) REFERENCES workflow_runs(repository, id) ON DELETE CASCADE
);
CREATE INDEX idx_workflow_jobs_run ON workflow_jobs(repository, run_id);
CREATE INDEX idx_workflow_jobs_updated ON workflow_jobs(repository, updated_at DESC);

CREATE TABLE workflow_steps (
	repository   TEXT NOT NULL,
	job_id       INTEGER NOT NULL,
	number       INTEGER NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT '',
	conclusion   TEXT NOT NULL DEFAULT '',
	started_at   INTEGER,
	completed_at INTEGER,
	PRIMARY KEY (repository, job_id, number),
	FOREIGN KEY (repository, job_id) REFERENCES workflow_jobs(repository, id) ON DELETE CASCADE
);

CREATE TABLE webhook_deliveries (
	id           TEXT PRIMARY KEY,
	event        TEXT NOT NULL,
	action       TEXT NOT NULL DEFAULT '',
	repository   TEXT NOT NULL DEFAULT '',
	received_at  INTEGER NOT NULL,
	processed_at INTEGER,
	payload      BLOB
);
CREATE INDEX idx_webhook_deliveries_received ON webhook_deliveries(received_at DESC);

CREATE TABLE sync_state (
	key        TEXT PRIMARY KEY,
	cursor     TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
`},
	{version: 2, sql: `
ALTER TABLE runner_sessions ADD COLUMN target TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_sessions ADD COLUMN run_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_sessions ADD COLUMN github_registration_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_sessions ADD COLUMN backend_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_sessions ADD COLUMN attribution_source TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_sessions ADD COLUMN attribution_confidence INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_sessions ADD COLUMN planned_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_sessions ADD COLUMN registered_at INTEGER;
ALTER TABLE runner_sessions ADD COLUMN launched_at INTEGER;
ALTER TABLE runner_sessions ADD COLUMN exit_code INTEGER NOT NULL DEFAULT 0;
UPDATE runner_sessions SET planned_at = started_at WHERE planned_at = 0;

ALTER TABLE workflow_runs ADD COLUMN workflow_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_runs ADD COLUMN workflow_path TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD COLUMN display_title TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD COLUMN triggering_actor TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD COLUMN api_url TEXT NOT NULL DEFAULT '';

ALTER TABLE workflow_jobs ADD COLUMN run_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_jobs ADD COLUMN workflow_name TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_jobs ADD COLUMN head_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_jobs ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_jobs ADD COLUMN labels_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE workflow_jobs ADD COLUMN runner_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_jobs ADD COLUMN pool_name TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_jobs ADD COLUMN local_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_jobs ADD COLUMN attribution_source TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_jobs ADD COLUMN attribution_confidence INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_jobs ADD COLUMN api_url TEXT NOT NULL DEFAULT '';

ALTER TABLE sync_state ADD COLUMN repository TEXT NOT NULL DEFAULT '';
ALTER TABLE sync_state ADD COLUMN phase TEXT NOT NULL DEFAULT '';
ALTER TABLE sync_state ADD COLUMN last_success_at INTEGER;
ALTER TABLE sync_state ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE sync_state ADD COLUMN retry_after INTEGER;
ALTER TABLE sync_state ADD COLUMN backfill_complete INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sync_state ADD COLUMN rate_remaining INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sync_state ADD COLUMN rate_reset_at INTEGER;

CREATE INDEX idx_runner_sessions_runner_name ON runner_sessions(runner_name);
CREATE INDEX idx_runner_sessions_pool_started ON runner_sessions(pool_name, started_at DESC);
CREATE INDEX idx_workflow_runs_status_updated ON workflow_runs(status, updated_at DESC);
CREATE INDEX idx_workflow_runs_workflow_created ON workflow_runs(workflow_name, created_at DESC);
CREATE INDEX idx_workflow_jobs_conclusion_completed ON workflow_jobs(conclusion, completed_at DESC);
CREATE INDEX idx_workflow_jobs_pool_completed ON workflow_jobs(pool_name, completed_at DESC);
CREATE INDEX idx_workflow_jobs_runner_name ON workflow_jobs(runner_name);
CREATE INDEX idx_workflow_jobs_attribution ON workflow_jobs(attribution_source, completed_at DESC);
CREATE INDEX idx_workflow_jobs_session ON workflow_jobs(local_session_id);
CREATE INDEX idx_sync_state_repository ON sync_state(repository);
`},
	{version: 3, sql: `
CREATE TABLE hosts (
	id              TEXT PRIMARY KEY,
	installation_id TEXT NOT NULL UNIQUE,
	display_name    TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
);

CREATE TABLE host_epochs (
	id            TEXT PRIMARY KEY,
	host_id       TEXT NOT NULL,
	started_at    INTEGER NOT NULL,
	ended_at      INTEGER,
	last_sequence INTEGER NOT NULL DEFAULT 0,
	FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE
);
CREATE INDEX idx_host_epochs_host_started ON host_epochs(host_id, started_at DESC);

CREATE TABLE operational_events (
	id              TEXT PRIMARY KEY,
	schema_version  INTEGER NOT NULL,
	host_id         TEXT NOT NULL,
	host_epoch      TEXT NOT NULL,
	sequence        INTEGER NOT NULL,
	event_type      TEXT NOT NULL,
	entity_type     TEXT NOT NULL,
	entity_id       TEXT NOT NULL,
	timestamp       INTEGER NOT NULL,
	correlation_id  TEXT NOT NULL DEFAULT '',
	causation_id    TEXT NOT NULL DEFAULT '',
	actor_kind      TEXT NOT NULL,
	actor_id        TEXT NOT NULL DEFAULT '',
	payload_json    BLOB NOT NULL,
	command_id      TEXT NOT NULL DEFAULT '',
	previous_hash   TEXT NOT NULL DEFAULT '',
	integrity_hash  TEXT NOT NULL,
	UNIQUE (host_epoch, sequence),
	FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE,
	FOREIGN KEY (host_epoch) REFERENCES host_epochs(id) ON DELETE CASCADE
);
CREATE INDEX idx_operational_events_epoch_sequence ON operational_events(host_epoch, sequence);
CREATE INDEX idx_operational_events_entity ON operational_events(entity_type, entity_id, timestamp DESC);
CREATE INDEX idx_operational_events_type_timestamp ON operational_events(event_type, timestamp DESC);
CREATE INDEX idx_operational_events_correlation ON operational_events(correlation_id, timestamp DESC);
CREATE INDEX idx_operational_events_command ON operational_events(command_id, timestamp DESC);

CREATE TABLE projection_watermarks (
	name        TEXT PRIMARY KEY,
	host_epoch  TEXT NOT NULL,
	sequence    INTEGER NOT NULL,
	event_id    TEXT NOT NULL,
	updated_at  INTEGER NOT NULL,
	FOREIGN KEY (host_epoch) REFERENCES host_epochs(id) ON DELETE CASCADE,
	FOREIGN KEY (event_id) REFERENCES operational_events(id) ON DELETE CASCADE
);

CREATE TABLE health_snapshots (
	id          TEXT PRIMARY KEY,
	host_id     TEXT NOT NULL,
	host_epoch  TEXT NOT NULL,
	observed_at INTEGER NOT NULL,
	status      TEXT NOT NULL,
	payload_json BLOB NOT NULL,
	FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE,
	FOREIGN KEY (host_epoch) REFERENCES host_epochs(id) ON DELETE CASCADE
);
CREATE INDEX idx_health_snapshots_host_observed ON health_snapshots(host_id, observed_at DESC);
`},
	{version: 4, sql: `
CREATE TABLE runner_state (
	id            TEXT PRIMARY KEY,
	host_id       TEXT NOT NULL,
	host_epoch    TEXT NOT NULL,
	pool_name     TEXT NOT NULL DEFAULT '',
	target        TEXT NOT NULL DEFAULT '',
	repository    TEXT NOT NULL DEFAULT '',
	runner_name   TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	backend_id    TEXT NOT NULL DEFAULT '',
	error         TEXT NOT NULL DEFAULT '',
	last_event_id TEXT NOT NULL,
	sequence      INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE,
	FOREIGN KEY (host_epoch) REFERENCES host_epochs(id) ON DELETE CASCADE,
	FOREIGN KEY (last_event_id) REFERENCES operational_events(id) ON DELETE CASCADE
);
CREATE INDEX idx_runner_state_epoch_status ON runner_state(host_epoch, status, updated_at DESC);
CREATE INDEX idx_runner_state_pool_status ON runner_state(pool_name, status, updated_at DESC);
`},
	{version: 5, sql: `
CREATE TABLE commands (
	id                TEXT PRIMARY KEY,
	idempotency_scope TEXT NOT NULL,
	idempotency_key   TEXT NOT NULL,
	request_hash      TEXT NOT NULL,
	command_type      TEXT NOT NULL,
	command_version   INTEGER NOT NULL,
	host_id           TEXT NOT NULL,
	target_type       TEXT NOT NULL,
	target_id         TEXT NOT NULL,
	conflict_domain   TEXT NOT NULL,
	parameters_json   BLOB NOT NULL,
	actor_kind        TEXT NOT NULL,
	actor_id          TEXT NOT NULL,
	reason            TEXT NOT NULL DEFAULT '',
	confirmation      TEXT NOT NULL,
	state             TEXT NOT NULL,
	correlation_id    TEXT NOT NULL DEFAULT '',
	client_metadata_json BLOB NOT NULL,
	created_at        INTEGER NOT NULL,
	validated_at      INTEGER,
	queued_at         INTEGER,
	started_at        INTEGER,
	heartbeat_at      INTEGER,
	completed_at      INTEGER,
	timeout_at        INTEGER,
	before_state_json BLOB,
	outcome_json      BLOB,
	error             TEXT NOT NULL DEFAULT '',
	lease_owner       TEXT NOT NULL DEFAULT '',
	lease_expires_at  INTEGER,
	attempt           INTEGER NOT NULL DEFAULT 0,
	fencing_token     INTEGER NOT NULL DEFAULT 0,
	UNIQUE (idempotency_scope, idempotency_key)
);
CREATE INDEX idx_commands_state_created ON commands(state, created_at);
CREATE INDEX idx_commands_target_created ON commands(target_type, target_id, created_at DESC);
CREATE UNIQUE INDEX idx_commands_active_conflict_domain ON commands(conflict_domain)
	WHERE state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted');

CREATE TABLE command_transitions (
	id            TEXT PRIMARY KEY,
	command_id    TEXT NOT NULL,
	from_state    TEXT NOT NULL DEFAULT '',
	to_state      TEXT NOT NULL,
	occurred_at   INTEGER NOT NULL,
	actor_kind    TEXT NOT NULL,
	actor_id      TEXT NOT NULL,
	detail_json   BLOB NOT NULL,
	fencing_token INTEGER NOT NULL,
	FOREIGN KEY (command_id) REFERENCES commands(id) ON DELETE RESTRICT
);
CREATE INDEX idx_command_transitions_command ON command_transitions(command_id, occurred_at);

CREATE TABLE audit_events (
	id             TEXT PRIMARY KEY,
	command_id     TEXT NOT NULL DEFAULT '',
	occurred_at    INTEGER NOT NULL,
	actor_kind     TEXT NOT NULL,
	actor_id       TEXT NOT NULL,
	action         TEXT NOT NULL,
	target_type    TEXT NOT NULL,
	target_id      TEXT NOT NULL,
	correlation_id TEXT NOT NULL DEFAULT '',
	payload_json   BLOB NOT NULL,
	FOREIGN KEY (command_id) REFERENCES commands(id) ON DELETE RESTRICT
);
CREATE INDEX idx_audit_events_occurred ON audit_events(occurred_at DESC);
CREATE INDEX idx_audit_events_command ON audit_events(command_id, occurred_at);
`},
	{version: 6, sql: `
CREATE TABLE support_bundles (
	id           TEXT PRIMARY KEY,
	command_id   TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	from_at      INTEGER NOT NULL,
	to_at        INTEGER NOT NULL,
	file_name    TEXT NOT NULL,
	file_path    TEXT NOT NULL,
	size_bytes   INTEGER NOT NULL,
	sha256       TEXT NOT NULL,
	FOREIGN KEY (command_id) REFERENCES commands(id) ON DELETE RESTRICT
);
CREATE INDEX idx_support_bundles_expires ON support_bundles(expires_at);
CREATE INDEX idx_support_bundles_command ON support_bundles(command_id);
`},
	{version: 7, sql: `
CREATE VIRTUAL TABLE search_index USING fts5(
	entity_type UNINDEXED,
	entity_key UNINDEXED,
	repository,
	title,
	context,
	timestamp UNINDEXED,
	state UNINDEXED,
	tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER search_workflow_runs_insert AFTER INSERT ON workflow_runs BEGIN
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'run', NEW.repository || ':' || NEW.id, NEW.repository,
		trim(NEW.workflow_name || ' ' || NEW.display_title || ' ' || NEW.name),
		trim(NEW.event || ' ' || NEW.head_branch || ' ' || NEW.head_sha || ' ' ||
			NEW.actor || ' ' || NEW.triggering_actor || ' ' || NEW.workflow_path),
		NEW.created_at, COALESCE(NULLIF(NEW.conclusion, ''), NEW.status)
	);
END;
CREATE TRIGGER search_workflow_runs_update AFTER UPDATE ON workflow_runs BEGIN
	DELETE FROM search_index WHERE entity_type='run' AND entity_key=OLD.repository || ':' || OLD.id;
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'run', NEW.repository || ':' || NEW.id, NEW.repository,
		trim(NEW.workflow_name || ' ' || NEW.display_title || ' ' || NEW.name),
		trim(NEW.event || ' ' || NEW.head_branch || ' ' || NEW.head_sha || ' ' ||
			NEW.actor || ' ' || NEW.triggering_actor || ' ' || NEW.workflow_path),
		NEW.created_at, COALESCE(NULLIF(NEW.conclusion, ''), NEW.status)
	);
END;
CREATE TRIGGER search_workflow_runs_delete AFTER DELETE ON workflow_runs BEGIN
	DELETE FROM search_index WHERE entity_type='run' AND entity_key=OLD.repository || ':' || OLD.id;
END;

CREATE TRIGGER search_workflow_jobs_insert AFTER INSERT ON workflow_jobs BEGIN
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'job', NEW.repository || ':' || NEW.id, NEW.repository,
		trim(NEW.workflow_name || ' ' || NEW.name),
		trim(NEW.head_branch || ' ' || NEW.head_sha || ' ' || NEW.runner_name || ' ' ||
			NEW.runner_group || ' ' || NEW.pool_name || ' ' || NEW.labels_json),
		NEW.created_at, COALESCE(NULLIF(NEW.conclusion, ''), NEW.status)
	);
END;
CREATE TRIGGER search_workflow_jobs_update AFTER UPDATE ON workflow_jobs BEGIN
	DELETE FROM search_index WHERE entity_type='job' AND entity_key=OLD.repository || ':' || OLD.id;
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'job', NEW.repository || ':' || NEW.id, NEW.repository,
		trim(NEW.workflow_name || ' ' || NEW.name),
		trim(NEW.head_branch || ' ' || NEW.head_sha || ' ' || NEW.runner_name || ' ' ||
			NEW.runner_group || ' ' || NEW.pool_name || ' ' || NEW.labels_json),
		NEW.created_at, COALESCE(NULLIF(NEW.conclusion, ''), NEW.status)
	);
END;
CREATE TRIGGER search_workflow_jobs_delete AFTER DELETE ON workflow_jobs BEGIN
	DELETE FROM search_index WHERE entity_type='job' AND entity_key=OLD.repository || ':' || OLD.id;
END;

CREATE TRIGGER search_workflow_steps_insert AFTER INSERT ON workflow_steps BEGIN
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'step', NEW.repository || ':' || NEW.job_id || ':' || NEW.number,
		NEW.repository, NEW.name, 'workflow job step',
		COALESCE(NEW.started_at, 0), COALESCE(NULLIF(NEW.conclusion, ''), NEW.status)
	);
END;
CREATE TRIGGER search_workflow_steps_update AFTER UPDATE ON workflow_steps BEGIN
	DELETE FROM search_index WHERE entity_type='step'
		AND entity_key=OLD.repository || ':' || OLD.job_id || ':' || OLD.number;
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'step', NEW.repository || ':' || NEW.job_id || ':' || NEW.number,
		NEW.repository, NEW.name, 'workflow job step',
		COALESCE(NEW.started_at, 0), COALESCE(NULLIF(NEW.conclusion, ''), NEW.status)
	);
END;
CREATE TRIGGER search_workflow_steps_delete AFTER DELETE ON workflow_steps BEGIN
	DELETE FROM search_index WHERE entity_type='step'
		AND entity_key=OLD.repository || ':' || OLD.job_id || ':' || OLD.number;
END;

CREATE TRIGGER search_runner_sessions_insert AFTER INSERT ON runner_sessions BEGIN
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'runner', NEW.id, NEW.repository,
		trim(NEW.runner_name || ' ' || NEW.pool_name),
		trim(NEW.target || ' ' || NEW.backend_id || ' ' || NEW.error),
		NEW.started_at, COALESCE(NULLIF(NEW.conclusion, ''), NEW.status)
	);
END;
CREATE TRIGGER search_runner_sessions_update AFTER UPDATE ON runner_sessions BEGIN
	DELETE FROM search_index WHERE entity_type='runner' AND entity_key=OLD.id;
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'runner', NEW.id, NEW.repository,
		trim(NEW.runner_name || ' ' || NEW.pool_name),
		trim(NEW.target || ' ' || NEW.backend_id || ' ' || NEW.error),
		NEW.started_at, COALESCE(NULLIF(NEW.conclusion, ''), NEW.status)
	);
END;
CREATE TRIGGER search_runner_sessions_delete AFTER DELETE ON runner_sessions BEGIN
	DELETE FROM search_index WHERE entity_type='runner' AND entity_key=OLD.id;
END;

CREATE TRIGGER search_commands_insert AFTER INSERT ON commands BEGIN
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'command', NEW.id, '',
		trim(NEW.command_type || ' ' || NEW.target_type || ' ' || NEW.target_id),
		trim(NEW.reason || ' ' || NEW.actor_id), NEW.created_at, NEW.state
	);
END;
CREATE TRIGGER search_commands_update AFTER UPDATE ON commands BEGIN
	DELETE FROM search_index WHERE entity_type='command' AND entity_key=OLD.id;
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'command', NEW.id, '',
		trim(NEW.command_type || ' ' || NEW.target_type || ' ' || NEW.target_id),
		trim(NEW.reason || ' ' || NEW.actor_id), NEW.created_at, NEW.state
	);
END;
CREATE TRIGGER search_commands_delete AFTER DELETE ON commands BEGIN
	DELETE FROM search_index WHERE entity_type='command' AND entity_key=OLD.id;
END;

INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
SELECT 'run', repository || ':' || id, repository,
	trim(workflow_name || ' ' || display_title || ' ' || name),
	trim(event || ' ' || head_branch || ' ' || head_sha || ' ' || actor || ' ' ||
		triggering_actor || ' ' || workflow_path),
	created_at, COALESCE(NULLIF(conclusion, ''), status)
FROM workflow_runs;
INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
SELECT 'job', repository || ':' || id, repository,
	trim(workflow_name || ' ' || name),
	trim(head_branch || ' ' || head_sha || ' ' || runner_name || ' ' ||
		runner_group || ' ' || pool_name || ' ' || labels_json),
	created_at, COALESCE(NULLIF(conclusion, ''), status)
FROM workflow_jobs;
INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
SELECT 'step', repository || ':' || job_id || ':' || number, repository,
	name, 'workflow job step', COALESCE(started_at, 0),
	COALESCE(NULLIF(conclusion, ''), status)
FROM workflow_steps;
INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
SELECT 'runner', id, repository, trim(runner_name || ' ' || pool_name),
	trim(target || ' ' || backend_id || ' ' || error), started_at,
	COALESCE(NULLIF(conclusion, ''), status)
FROM runner_sessions;
INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
SELECT 'command', id, '', trim(command_type || ' ' || target_type || ' ' || target_id),
	trim(reason || ' ' || actor_id), created_at, state
FROM commands;
`},
	{version: 8, sql: `
CREATE TABLE access_audit_events (
	id             TEXT PRIMARY KEY,
	occurred_at    INTEGER NOT NULL,
	actor_kind     TEXT NOT NULL,
	actor_id       TEXT NOT NULL,
	action         TEXT NOT NULL,
	target_type    TEXT NOT NULL,
	target_id      TEXT NOT NULL,
	correlation_id TEXT NOT NULL DEFAULT '',
	outcome        TEXT NOT NULL,
	byte_count     INTEGER NOT NULL DEFAULT 0,
	duration_ms    INTEGER NOT NULL DEFAULT 0,
	error_code     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_access_audit_occurred ON access_audit_events(occurred_at DESC);
CREATE INDEX idx_access_audit_target ON access_audit_events(target_type, target_id, occurred_at DESC);
`},
	{version: 9, sql: `
CREATE TABLE saved_views (
	id              TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL UNIQUE,
	request_hash    TEXT NOT NULL,
	name            TEXT NOT NULL COLLATE NOCASE UNIQUE,
	query           TEXT NOT NULL,
	entity_type     TEXT NOT NULL DEFAULT '',
	repository      TEXT NOT NULL DEFAULT '',
	state           TEXT NOT NULL DEFAULT '',
	created_by      TEXT NOT NULL,
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	version         INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_saved_views_name ON saved_views(name);

CREATE TABLE saved_view_deletions (
	idempotency_key TEXT PRIMARY KEY,
	saved_view_id   TEXT NOT NULL,
	version         INTEGER NOT NULL,
	deleted_at      INTEGER NOT NULL
);

CREATE TABLE generated_exports (
	id              TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL UNIQUE,
	request_hash    TEXT NOT NULL,
	created_at      INTEGER NOT NULL,
	expires_at      INTEGER NOT NULL,
	file_name       TEXT NOT NULL,
	file_path       TEXT NOT NULL,
	size_bytes      INTEGER NOT NULL,
	sha256          TEXT NOT NULL,
	record_count    INTEGER NOT NULL,
	query           TEXT NOT NULL,
	entity_type     TEXT NOT NULL DEFAULT '',
	repository      TEXT NOT NULL DEFAULT '',
	state           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_generated_exports_expires ON generated_exports(expires_at);
`},
	{version: 10, sql: `
CREATE TABLE alert_rules (
	id               TEXT PRIMARY KEY,
	version          INTEGER NOT NULL DEFAULT 1,
	name             TEXT NOT NULL,
	family           TEXT NOT NULL,
	severity         TEXT NOT NULL,
	event_type       TEXT NOT NULL,
	hold_seconds     INTEGER NOT NULL DEFAULT 0,
	cooldown_seconds INTEGER NOT NULL DEFAULT 900,
	enabled          INTEGER NOT NULL DEFAULT 1,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
);
CREATE INDEX idx_alert_rules_event ON alert_rules(enabled, event_type);

INSERT INTO alert_rules(
	id, name, family, severity, event_type, hold_seconds, cooldown_seconds,
	created_at, updated_at
) VALUES
	('runner-stuck', 'Runner provisioning is stuck', 'runner', 'high',
		'runner.planned', 600, 900, 0, 0),
	('runner-failed', 'Runner provisioning failed', 'runner', 'high',
		'runner.failed', 0, 900, 0, 0),
	('command-failed', 'Operator command failed', 'command', 'medium',
		'command.failed', 0, 900, 0, 0),
	('command-interrupted', 'Operator command was interrupted', 'command', 'medium',
		'command.interrupted', 0, 900, 0, 0),
	('command-unknown', 'Operator command outcome is unknown', 'command', 'high',
		'command.unknown_outcome', 0, 900, 0, 0);

CREATE TABLE alert_instances (
	id                TEXT PRIMARY KEY,
	rule_id           TEXT NOT NULL,
	dedup_key         TEXT NOT NULL,
	state             TEXT NOT NULL,
	severity          TEXT NOT NULL,
	summary           TEXT NOT NULL,
	details_json      BLOB NOT NULL,
	source_event_id   TEXT NOT NULL,
	first_observed_at INTEGER NOT NULL,
	last_observed_at  INTEGER NOT NULL,
	due_at            INTEGER NOT NULL,
	opened_at         INTEGER,
	resolved_at       INTEGER,
	acknowledged_at   INTEGER,
	acknowledged_by   TEXT NOT NULL DEFAULT '',
	silenced_until    INTEGER,
	last_notified_at  INTEGER,
	occurrence_count  INTEGER NOT NULL DEFAULT 1,
	version           INTEGER NOT NULL DEFAULT 1,
	FOREIGN KEY (rule_id) REFERENCES alert_rules(id) ON DELETE RESTRICT,
	UNIQUE (rule_id, dedup_key)
);
CREATE INDEX idx_alert_instances_state_due ON alert_instances(state, due_at);
CREATE INDEX idx_alert_instances_observed ON alert_instances(last_observed_at DESC);

CREATE TABLE alert_evaluated_events (
	event_id     TEXT NOT NULL,
	rule_id      TEXT NOT NULL,
	alert_id     TEXT NOT NULL,
	evaluated_at INTEGER NOT NULL,
	PRIMARY KEY (event_id, rule_id),
	FOREIGN KEY (rule_id) REFERENCES alert_rules(id) ON DELETE RESTRICT,
	FOREIGN KEY (alert_id) REFERENCES alert_instances(id) ON DELETE CASCADE
);
CREATE INDEX idx_alert_evaluated_alert ON alert_evaluated_events(alert_id);

CREATE TABLE alert_transitions (
	id             TEXT PRIMARY KEY,
	alert_id       TEXT NOT NULL,
	from_state     TEXT NOT NULL DEFAULT '',
	to_state       TEXT NOT NULL,
	occurred_at    INTEGER NOT NULL,
	actor_kind     TEXT NOT NULL,
	actor_id       TEXT NOT NULL,
	reason         TEXT NOT NULL DEFAULT '',
	detail_json    BLOB NOT NULL,
	correlation_id TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (alert_id) REFERENCES alert_instances(id) ON DELETE RESTRICT
);
CREATE INDEX idx_alert_transitions_alert ON alert_transitions(alert_id, occurred_at);

CREATE TABLE alert_silences (
	id             TEXT PRIMARY KEY,
	alert_id       TEXT NOT NULL,
	starts_at      INTEGER NOT NULL,
	expires_at     INTEGER NOT NULL,
	created_at     INTEGER NOT NULL,
	created_by     TEXT NOT NULL,
	reason         TEXT NOT NULL,
	correlation_id TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (alert_id) REFERENCES alert_instances(id) ON DELETE RESTRICT
);
CREATE INDEX idx_alert_silences_active ON alert_silences(alert_id, expires_at);

CREATE TABLE alert_annotations (
	id             TEXT PRIMARY KEY,
	alert_id       TEXT NOT NULL,
	body           TEXT NOT NULL,
	created_at     INTEGER NOT NULL,
	created_by     TEXT NOT NULL,
	correlation_id TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (alert_id) REFERENCES alert_instances(id) ON DELETE RESTRICT
);
CREATE INDEX idx_alert_annotations_alert ON alert_annotations(alert_id, created_at);

CREATE TABLE alert_mutations (
	idempotency_scope TEXT NOT NULL,
	idempotency_key   TEXT NOT NULL,
	request_hash      TEXT NOT NULL,
	action            TEXT NOT NULL,
	alert_id          TEXT NOT NULL,
	result_id         TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	PRIMARY KEY (idempotency_scope, idempotency_key),
	FOREIGN KEY (alert_id) REFERENCES alert_instances(id) ON DELETE RESTRICT
);
CREATE INDEX idx_alert_mutations_alert ON alert_mutations(alert_id, created_at);

CREATE TABLE alert_audit_events (
	id             TEXT PRIMARY KEY,
	alert_id       TEXT NOT NULL,
	occurred_at    INTEGER NOT NULL,
	actor_kind     TEXT NOT NULL,
	actor_id       TEXT NOT NULL,
	action         TEXT NOT NULL,
	correlation_id TEXT NOT NULL DEFAULT '',
	payload_json   BLOB NOT NULL,
	FOREIGN KEY (alert_id) REFERENCES alert_instances(id) ON DELETE RESTRICT
);
CREATE INDEX idx_alert_audit_occurred ON alert_audit_events(occurred_at DESC);
CREATE INDEX idx_alert_audit_alert ON alert_audit_events(alert_id, occurred_at);

CREATE TABLE notification_endpoints (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL UNIQUE,
	kind         TEXT NOT NULL,
	config_ref   TEXT NOT NULL UNIQUE,
	enabled      INTEGER NOT NULL DEFAULT 1,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);

CREATE TABLE notification_deliveries (
	id               TEXT PRIMARY KEY,
	alert_id         TEXT NOT NULL,
	transition_id    TEXT NOT NULL,
	endpoint_id      TEXT NOT NULL,
	dedup_key        TEXT NOT NULL UNIQUE,
	payload_json     BLOB NOT NULL,
	state            TEXT NOT NULL,
	attempt          INTEGER NOT NULL DEFAULT 0,
	next_attempt_at  INTEGER NOT NULL,
	lease_owner      TEXT NOT NULL DEFAULT '',
	lease_expires_at INTEGER,
	last_error       TEXT NOT NULL DEFAULT '',
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	delivered_at     INTEGER,
	dead_lettered_at INTEGER,
	FOREIGN KEY (alert_id) REFERENCES alert_instances(id) ON DELETE RESTRICT,
	FOREIGN KEY (transition_id) REFERENCES alert_transitions(id) ON DELETE RESTRICT,
	FOREIGN KEY (endpoint_id) REFERENCES notification_endpoints(id) ON DELETE RESTRICT
);
CREATE INDEX idx_notification_deliveries_ready
	ON notification_deliveries(state, next_attempt_at);
CREATE INDEX idx_notification_deliveries_alert
	ON notification_deliveries(alert_id, created_at);

CREATE TRIGGER alert_instances_search_ai AFTER INSERT ON alert_instances BEGIN
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'alert', new.id, '', new.summary,
		trim(new.rule_id || ' ' || new.dedup_key || ' ' || new.severity || ' ' ||
			new.acknowledged_by),
		new.last_observed_at, new.state
	);
END;
CREATE TRIGGER alert_instances_search_au AFTER UPDATE ON alert_instances BEGIN
	DELETE FROM search_index WHERE entity_type='alert' AND entity_key=old.id;
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'alert', new.id, '', new.summary,
		trim(new.rule_id || ' ' || new.dedup_key || ' ' || new.severity || ' ' ||
			new.acknowledged_by),
		new.last_observed_at, new.state
	);
END;
CREATE TRIGGER alert_instances_search_ad AFTER DELETE ON alert_instances BEGIN
	DELETE FROM search_index WHERE entity_type='alert' AND entity_key=old.id;
END;

CREATE TRIGGER alert_annotations_search_ai AFTER INSERT ON alert_annotations BEGIN
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'annotation', new.alert_id || ':' || new.id, '', new.body,
		trim(new.alert_id || ' ' || new.created_by), new.created_at, 'recorded'
	);
END;
CREATE TRIGGER alert_annotations_search_au AFTER UPDATE ON alert_annotations BEGIN
	DELETE FROM search_index
		WHERE entity_type='annotation' AND entity_key=old.alert_id || ':' || old.id;
	INSERT INTO search_index(entity_type, entity_key, repository, title, context, timestamp, state)
	VALUES (
		'annotation', new.alert_id || ':' || new.id, '', new.body,
		trim(new.alert_id || ' ' || new.created_by), new.created_at, 'recorded'
	);
END;
CREATE TRIGGER alert_annotations_search_ad AFTER DELETE ON alert_annotations BEGIN
	DELETE FROM search_index
		WHERE entity_type='annotation' AND entity_key=old.alert_id || ':' || old.id;
END;
`},
	{version: 11, sql: `
CREATE TABLE backups (
	id                   TEXT PRIMARY KEY,
	command_id           TEXT NOT NULL UNIQUE,
	purpose              TEXT NOT NULL,
	state                TEXT NOT NULL,
	created_at           INTEGER NOT NULL,
	completed_at         INTEGER,
	expires_at           INTEGER NOT NULL,
	file_name            TEXT NOT NULL,
	file_path            TEXT NOT NULL,
	manifest_path        TEXT NOT NULL,
	size_bytes           INTEGER NOT NULL DEFAULT 0,
	sha256               TEXT NOT NULL DEFAULT '',
	schema_version       INTEGER NOT NULL,
	app_version          TEXT NOT NULL,
	host_id              TEXT NOT NULL,
	audit_event_id       TEXT NOT NULL DEFAULT '',
	audit_integrity_hash TEXT NOT NULL DEFAULT '',
	quick_check          TEXT NOT NULL DEFAULT '',
	error                TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE RESTRICT
);
CREATE INDEX idx_backups_created ON backups(created_at DESC);
CREATE INDEX idx_backups_expiry ON backups(state, expires_at);
`},
	{version: 12, sql: `
CREATE TABLE restores (
	id              TEXT PRIMARY KEY,
	command_id      TEXT NOT NULL UNIQUE,
	backup_id       TEXT NOT NULL,
	state           TEXT NOT NULL,
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	activated_at    INTEGER,
	completed_at    INTEGER,
	host_id         TEXT NOT NULL,
	schema_version  INTEGER NOT NULL,
	size_bytes      INTEGER NOT NULL,
	sha256          TEXT NOT NULL,
	database_path   TEXT NOT NULL,
	staged_path     TEXT NOT NULL,
	rollback_path   TEXT NOT NULL,
	handoff_path    TEXT NOT NULL,
	quick_check     TEXT NOT NULL DEFAULT '',
	rollback_reason TEXT NOT NULL DEFAULT '',
	error           TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE RESTRICT
);
CREATE INDEX idx_restores_created ON restores(created_at DESC);
CREATE INDEX idx_restores_state ON restores(state, updated_at DESC);
INSERT INTO alert_rules (
	id, name, family, severity, event_type, hold_seconds, cooldown_seconds,
	created_at, updated_at
) VALUES (
	'restore-rolled-back', 'Database restore rolled back', 'recovery', 'high',
	'restore.rolled_back', 0, 3600, 0, 0
);
`},
	{version: 13, sql: `
CREATE TABLE updates (
	id              TEXT PRIMARY KEY,
	command_id      TEXT NOT NULL UNIQUE,
	state           TEXT NOT NULL,
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	activated_at    INTEGER,
	completed_at    INTEGER,
	version         TEXT NOT NULL,
	commit_hash     TEXT NOT NULL,
	host_id         TEXT NOT NULL,
	target_path     TEXT NOT NULL,
	size_bytes      INTEGER NOT NULL,
	sha256          TEXT NOT NULL,
	schema_min      INTEGER NOT NULL,
	schema_max      INTEGER NOT NULL,
	api_version     TEXT NOT NULL,
	artifact_path   TEXT NOT NULL,
	handoff_path    TEXT NOT NULL,
	rollback_reason TEXT NOT NULL DEFAULT '',
	error           TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE RESTRICT
);
CREATE INDEX idx_updates_created ON updates(created_at DESC);
CREATE INDEX idx_updates_state ON updates(state, updated_at DESC);
INSERT INTO alert_rules (
	id, name, family, severity, event_type, hold_seconds, cooldown_seconds,
	created_at, updated_at
) VALUES (
	'update-rolled-back', 'Application update rolled back', 'recovery', 'high',
	'update.rolled_back', 0, 3600, 0, 0
);
`},
	{version: 14, sql: `
DROP INDEX IF EXISTS idx_operational_events_epoch_sequence;
DROP INDEX IF EXISTS idx_saved_views_name;

CREATE INDEX idx_operational_events_host ON operational_events(host_id);
CREATE INDEX idx_projection_watermarks_epoch ON projection_watermarks(host_epoch);
CREATE INDEX idx_projection_watermarks_event ON projection_watermarks(event_id);
CREATE INDEX idx_health_snapshots_epoch ON health_snapshots(host_epoch);
CREATE INDEX idx_runner_state_host ON runner_state(host_id);
CREATE INDEX idx_runner_state_last_event ON runner_state(last_event_id);
CREATE INDEX idx_alert_evaluated_rule ON alert_evaluated_events(rule_id);
CREATE INDEX idx_notification_deliveries_transition ON notification_deliveries(transition_id);
CREATE INDEX idx_notification_deliveries_endpoint ON notification_deliveries(endpoint_id);
CREATE INDEX idx_backups_host ON backups(host_id);
CREATE INDEX idx_restores_host ON restores(host_id);
CREATE INDEX idx_updates_host ON updates(host_id);
CREATE INDEX idx_operational_events_epoch_timestamp
	ON operational_events(host_epoch, timestamp, sequence);
CREATE INDEX idx_health_snapshots_observed ON health_snapshots(observed_at);
CREATE INDEX idx_workflow_runs_completed ON workflow_runs(completed_at);
CREATE INDEX idx_commands_completed ON commands(completed_at);
CREATE INDEX idx_command_transitions_occurred ON command_transitions(occurred_at);
CREATE INDEX idx_restores_completed ON restores(completed_at);
CREATE INDEX idx_updates_completed ON updates(completed_at);

CREATE TABLE operational_event_anchors (
	host_epoch       TEXT PRIMARY KEY,
	through_sequence INTEGER NOT NULL,
	integrity_hash   TEXT NOT NULL,
	compacted_at     INTEGER NOT NULL,
	FOREIGN KEY (host_epoch) REFERENCES host_epochs(id) ON DELETE CASCADE
);
`},
	{version: 15, sql: `
CREATE INDEX idx_runner_sessions_started_id
	ON runner_sessions(started_at DESC, id DESC);
CREATE INDEX idx_runner_sessions_repository_started_id
	ON runner_sessions(repository, started_at DESC, id DESC);
CREATE INDEX idx_workflow_runs_created_id
	ON workflow_runs(created_at DESC, id DESC);
CREATE INDEX idx_workflow_runs_repository_created_id
	ON workflow_runs(repository, created_at DESC, id DESC);
CREATE INDEX idx_workflow_jobs_analytics_cover
	ON workflow_jobs(
		created_at, repository, workflow_name, conclusion,
		attribution_source, started_at, completed_at
	);
CREATE INDEX idx_runner_sessions_repository_completed
	ON runner_sessions(repository, completed_at);
CREATE INDEX idx_workflow_jobs_repository_conclusion
	ON workflow_jobs(repository, conclusion);

CREATE TABLE analytics_state (
	id             INTEGER PRIMARY KEY CHECK (id = 1),
	source_version INTEGER NOT NULL
);
INSERT INTO analytics_state (id, source_version) VALUES (1, 0);

CREATE TABLE analytics_snapshots (
	cache_key      TEXT PRIMARY KEY,
	source_version INTEGER NOT NULL,
	generated_at   INTEGER NOT NULL,
	report_json    BLOB NOT NULL
);
CREATE INDEX idx_analytics_snapshots_generated
	ON analytics_snapshots(generated_at DESC);
`},
}

func migrate(ctx context.Context, db *sql.DB) error {
	state, err := inspectMigrationLedger(ctx, db)
	if err != nil {
		return err
	}
	if !state.exists {
		if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL,
			checksum   TEXT NOT NULL DEFAULT ''
		)`); err != nil {
			return fmt.Errorf("create schema migrations: %w", err)
		}
	} else if !state.hasChecksum || state.blankChecksums > 0 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration ledger upgrade: %w", err)
		}
		if !state.hasChecksum {
			if _, err = tx.ExecContext(ctx,
				`ALTER TABLE schema_migrations ADD COLUMN checksum TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("add migration checksum: %w", err)
			}
		}
		for _, m := range migrations {
			if m.version > state.latest {
				break
			}
			if _, err = tx.ExecContext(ctx,
				`UPDATE schema_migrations SET checksum=? WHERE version=? AND checksum=''`,
				m.checksum(), m.version); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("backfill migration %d checksum: %w", m.version, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration ledger upgrade: %w", err)
		}
	}

	for _, m := range migrations {
		var exists int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, m.version).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read migration %d: %w", m.version, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err = tx.ExecContext(ctx, m.sql); err == nil {
			if m.version == 14 {
				err = rehashOperationalEventsTx(ctx, tx)
			}
		}
		if err == nil {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO schema_migrations(version, applied_at, checksum) VALUES(?, ?, ?)`,
				m.version, timeMillis(nowUTC()), m.checksum())
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	_, err = inspectMigrationLedger(ctx, db)
	return err
}

func (m migration) checksum() string {
	input := m.sql
	if m.version == 14 {
		input += "\n-- go-hook:rehash-operational-events-v1"
	}
	sum := sha256.Sum256([]byte(input))
	return "sha256:" + hex.EncodeToString(sum[:])
}
