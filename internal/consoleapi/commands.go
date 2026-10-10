package consoleapi

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/runtimecontrol"
	"github.com/GerardSmit/multirunner/internal/update"
)

const maxCommandBody = 64 << 10

type commandRequest struct {
	Type         string          `json:"type"`
	TargetType   string          `json:"target_type"`
	TargetID     string          `json:"target_id"`
	Parameters   json.RawMessage `json:"parameters"`
	Reason       string          `json:"reason"`
	Confirmation string          `json:"confirmation"`
}

type commandPlan struct {
	ConflictDomain     string `json:"conflict_domain"`
	ConfirmationPhrase string `json:"confirmation_phrase,omitempty"`
	ReasonRequired     bool   `json:"reason_required"`
	Impact             string `json:"impact"`
}

func commandPreviewHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validMutationRequest(r, options) {
			writeError(w, http.StatusForbidden, "csrf_failed", "preview request origin or CSRF state is invalid")
			return
		}
		input, ok := decodeCommandRequest(w, r)
		if !ok {
			return
		}
		plan, err := planCommand(input)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		if requiresRecentPairing(plan) && !options.Auth.ValidateRecentPairing(r) {
			writeStepUpRequired(w)
			return
		}
		candidate := control.Command{
			Type: input.Type, HostID: options.HostID,
			TargetType: input.TargetType, TargetID: input.TargetID,
			ConflictDomain: plan.ConflictDomain, Parameters: normalizedParameters(input.Parameters),
		}
		if err := options.Controls.Validate(candidate); err != nil {
			writeControlValidationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
	}
}

func commandCreateHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validMutationRequest(r, options) {
			writeError(w, http.StatusForbidden, "csrf_failed", "mutation request origin or CSRF state is invalid")
			return
		}
		idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if idempotencyKey == "" || len(idempotencyKey) > 128 {
			writeError(w, http.StatusBadRequest, "validation_failed", "Idempotency-Key must contain 1 to 128 characters")
			return
		}
		input, ok := decodeCommandRequest(w, r)
		if !ok {
			return
		}
		plan, err := planCommand(input)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		if requiresRecentPairing(plan) && !options.Auth.ValidateRecentPairing(r) {
			writeStepUpRequired(w)
			return
		}
		request, err := buildControlRequest(r, options, input, idempotencyKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		candidate := control.Command{
			Type: request.Type, HostID: request.HostID,
			TargetType: request.TargetType, TargetID: request.TargetID,
			ConflictDomain: request.ConflictDomain, Parameters: request.Parameters,
		}
		if err := options.Controls.Validate(candidate); err != nil {
			writeControlValidationError(w, err)
			return
		}
		command, created, err := options.Commands.CreateCommand(r.Context(), request)
		switch {
		case errors.Is(err, control.ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input")
			return
		case errors.Is(err, control.ErrConflictDomainBusy):
			writeError(w, http.StatusConflict, "state_conflict", "another command owns the target conflict domain")
			return
		case err != nil:
			writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "command intent could not be persisted")
			return
		}
		if !created {
			writeJSON(w, http.StatusOK, command)
			return
		}
		writeJSON(w, http.StatusAccepted, command)
	}
}

func requiresRecentPairing(plan commandPlan) bool {
	return plan.ConfirmationPhrase != ""
}

func writeStepUpRequired(w http.ResponseWriter) {
	writeErrorDetails(
		w, http.StatusForbidden, "step_up_required",
		"This sensitive action requires a recent console pairing. Run `multirunner console open` on this host and retry.",
		nil, map[string]any{"pairing_required": true},
	)
}

func commandReadHandler(options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		command, err := options.Commands.Command(r.Context(), r.PathValue("id"))
		if errors.Is(err, control.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "command was not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "commands_unavailable", "command state is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, command)
	}
}

func buildControlRequest(
	r *http.Request, options Options, input commandRequest, idempotencyKey string,
) (control.Request, error) {
	input.Type = strings.TrimSpace(input.Type)
	input.TargetType = strings.TrimSpace(input.TargetType)
	input.TargetID = strings.TrimSpace(input.TargetID)
	input.Reason = strings.TrimSpace(input.Reason)
	if len(input.Reason) > 500 {
		return control.Request{}, errors.New("reason cannot exceed 500 characters")
	}
	input.Parameters = normalizedParameters(input.Parameters)
	plan, err := planCommand(input)
	if err != nil {
		return control.Request{}, err
	}
	if plan.ReasonRequired && input.Reason == "" {
		return control.Request{}, errors.New("a reason is required for this command")
	}
	if plan.ConfirmationPhrase != "" && input.Confirmation != plan.ConfirmationPhrase {
		return control.Request{}, errors.New("confirmation phrase does not match the requested command")
	}
	confirmation := control.ConfirmationNotRequired
	if plan.ConfirmationPhrase != "" {
		confirmation = control.ConfirmationConfirmed
	}
	session, ok := consoleauth.SessionFromContext(r.Context())
	if !ok {
		return control.Request{}, errors.New("authenticated session is unavailable")
	}
	metadata, _ := json.Marshal(map[string]string{
		"remote_address": clientAddress(r.RemoteAddr),
		"user_agent":     truncate(r.UserAgent(), 256),
	})
	return control.Request{
		IdempotencyKey: idempotencyKey, Type: input.Type,
		Version: control.CurrentCommandVersion, HostID: options.HostID,
		TargetType: input.TargetType, TargetID: input.TargetID,
		ConflictDomain: plan.ConflictDomain, Parameters: input.Parameters,
		ActorKind: operations.ActorOperator, ActorID: session.ActorID,
		Reason: input.Reason, Confirmation: confirmation,
		CorrelationID: wCorrelationID(r), ClientMetadata: metadata,
		TimeoutAt: commandTimeout(input.Type),
	}, nil
}

func planCommand(input commandRequest) (commandPlan, error) {
	input.Type = strings.TrimSpace(input.Type)
	input.TargetType = strings.TrimSpace(input.TargetType)
	input.TargetID = strings.TrimSpace(input.TargetID)
	var plan commandPlan
	switch input.Type {
	case runtimecontrol.CommandPoolPause, runtimecontrol.CommandPoolResume,
		runtimecontrol.CommandPoolCancelDrain:
		if input.TargetType != "pool" || input.TargetID == "" {
			return commandPlan{}, errors.New("pool commands require a pool target")
		}
		plan.ConflictDomain = "pool:" + input.TargetID
		switch input.Type {
		case runtimecontrol.CommandPoolPause:
			plan.Impact = "Prevent new runners from being provisioned; active jobs continue."
		default:
			plan.Impact = "Allow new runners to be provisioned for this pool."
		}
	case runtimecontrol.CommandPoolDrain:
		if input.TargetType != "pool" || input.TargetID == "" {
			return commandPlan{}, errors.New("pool drain requires a pool target")
		}
		plan.ConflictDomain = "pool:" + input.TargetID
		plan.ConfirmationPhrase = "drain " + input.TargetID
		plan.ReasonRequired = true
		plan.Impact = "Pause provisioning and wait for every active runner in this pool to finish."
	case runtimecontrol.CommandRunnerTerminate, runtimecontrol.CommandRunnerRecycle:
		if input.TargetType != "runner" || input.TargetID == "" {
			return commandPlan{}, errors.New("runner commands require a runner target")
		}
		var parameters struct {
			Pool string `json:"pool"`
		}
		if err := json.Unmarshal(normalizedParameters(input.Parameters), &parameters); err != nil ||
			strings.TrimSpace(parameters.Pool) == "" {
			return commandPlan{}, errors.New("runner commands require a pool parameter")
		}
		plan.ConflictDomain = "pool:" + strings.TrimSpace(parameters.Pool)
		verb := strings.TrimPrefix(input.Type, "runner.")
		plan.ConfirmationPhrase = verb + " " + input.TargetID
		plan.ReasonRequired = true
		plan.Impact = "Stop this active runner through its owned cleanup path; its current job may be interrupted."
	case runtimecontrol.CommandHistorySync:
		if input.TargetType != "system" || input.TargetID != "history" {
			return commandPlan{}, errors.New("history sync requires the system history target")
		}
		plan.ConflictDomain = "history"
		plan.Impact = "Request an immediate GitHub history reconciliation pass."
	case runtimecontrol.CommandSupportBundle:
		if input.TargetType != "system" || input.TargetID != "support-bundles" {
			return commandPlan{}, errors.New("support bundle generation requires the system support-bundles target")
		}
		plan.ConflictDomain = "support-bundles"
		plan.ConfirmationPhrase = "generate support bundle"
		plan.ReasonRequired = true
		plan.Impact = "Create a redacted local archive of diagnostics, configuration, database health, and bounded operational events."
	case runtimecontrol.CommandBackupCreate:
		if input.TargetType != "system" || input.TargetID != "backups" {
			return commandPlan{}, errors.New("backup creation requires the system backups target")
		}
		plan.ConflictDomain = "database-maintenance"
		plan.ConfirmationPhrase = "create verified backup"
		plan.ReasonRequired = true
		plan.Impact = "Create and integrity-check an online SQLite backup without stopping runner provisioning."
	case runtimecontrol.CommandRestoreStage:
		if input.TargetType != "system" || input.TargetID != "restore" {
			return commandPlan{}, errors.New("restore staging requires the system restore target")
		}
		var parameters restore.Request
		if err := json.Unmarshal(normalizedParameters(input.Parameters), &parameters); err != nil {
			return commandPlan{}, errors.New("restore parameters are invalid")
		}
		if err := restore.ValidateRequest(parameters); err != nil {
			return commandPlan{}, err
		}
		plan.ConflictDomain = "database-maintenance"
		plan.ConfirmationPhrase = "stage restore " + parameters.BackupID
		plan.ReasonRequired = true
		plan.Impact = "Stage a verified backup for activation on the next service restart. Startup automatically rolls back if the restored runtime does not become healthy."
	case runtimecontrol.CommandUpdateStage:
		if input.TargetType != "system" || input.TargetID != "updates" {
			return commandPlan{}, errors.New("update staging requires the system updates target")
		}
		var parameters update.Request
		if err := json.Unmarshal(normalizedParameters(input.Parameters), &parameters); err != nil {
			return commandPlan{}, errors.New("update parameters are invalid")
		}
		if err := update.ValidateRequest(parameters); err != nil {
			return commandPlan{}, err
		}
		plan.ConflictDomain = "host-maintenance"
		plan.ConfirmationPhrase = "stage trusted update"
		if parameters.Version != "" {
			plan.ConfirmationPhrase += " " + parameters.Version
		}
		plan.ReasonRequired = true
		plan.Impact = "Download and stage the target only after threshold signatures, provenance, compatibility, and downgrade policy pass. Activation occurs on service restart and automatically rolls back if the updated runtime does not become healthy."
	default:
		return commandPlan{}, errors.New("command type is not recognized")
	}
	return plan, nil
}

func validMutationRequest(r *http.Request, options Options) bool {
	if !options.Auth.ValidateCSRF(r) || r.Host != options.Listen {
		return false
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && origin.Scheme == "http" && origin.Host == r.Host &&
		origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
}

func writeCommandDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "validation_failed", "request body exceeds 65536 bytes")
		return
	}
	writeError(w, http.StatusBadRequest, "validation_failed", "request body is not a valid command")
}

func decodeCommandRequest(w http.ResponseWriter, r *http.Request) (commandRequest, bool) {
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "validation_failed", "Content-Type must be application/json")
		return commandRequest{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCommandBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input commandRequest
	if err := decoder.Decode(&input); err != nil {
		writeCommandDecodeError(w, err)
		return commandRequest{}, false
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "request body must contain one JSON object")
		return commandRequest{}, false
	}
	return input, true
}

func normalizedParameters(parameters json.RawMessage) json.RawMessage {
	if len(parameters) == 0 {
		return json.RawMessage(`{}`)
	}
	return parameters
}

func writeControlValidationError(w http.ResponseWriter, err error) {
	if errors.Is(err, control.ErrStateConflict) {
		writeError(w, http.StatusConflict, "state_conflict", err.Error())
		return
	}
	if errors.Is(err, runtimecontrol.ErrCapabilityUnsupported) {
		writeError(w, http.StatusConflict, "capability_unsupported", err.Error())
		return
	}
	writeValidationError(w, err)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func clientAddress(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		return host
	}
	return truncate(remote, 128)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func commandTimeout(commandType string) *time.Time {
	timeout := 2 * time.Minute
	switch commandType {
	case runtimecontrol.CommandPoolDrain:
		timeout = 30 * time.Minute
	case runtimecontrol.CommandSupportBundle:
		timeout = 5 * time.Minute
	case runtimecontrol.CommandBackupCreate:
		timeout = 15 * time.Minute
	case runtimecontrol.CommandRestoreStage:
		timeout = 15 * time.Minute
	case runtimecontrol.CommandUpdateStage:
		timeout = 30 * time.Minute
	}
	value := time.Now().UTC().Add(timeout)
	return &value
}

func wCorrelationID(r *http.Request) string {
	return r.Header.Get("X-Correlation-ID")
}
