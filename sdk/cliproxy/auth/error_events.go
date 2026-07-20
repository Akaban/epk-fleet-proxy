package auth

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
)

type errorEvent struct {
	Timestamp  time.Time            `json:"timestamp"`
	Provider   string               `json:"provider,omitempty"`
	Model      string               `json:"model,omitempty"`
	AuthID     string               `json:"auth_id,omitempty"`
	AuthIndex  string               `json:"auth_index"`
	StatusCode int                  `json:"status_code"`
	Body       string               `json:"body"`
	Code       string               `json:"code,omitempty"`
	Retryable  bool                 `json:"retryable,omitempty"`
	AuthStatus errorEventAuthStatus `json:"auth_status"`
}

type authStateEvent struct {
	V          int                  `json:"v"`
	Kind       string               `json:"kind"`
	EventID    string               `json:"event_id"`
	Timestamp  time.Time            `json:"timestamp"`
	Provider   string               `json:"provider,omitempty"`
	Model      string               `json:"model,omitempty"`
	AuthID     string               `json:"auth_id"`
	Account    string               `json:"account"`
	Email      string               `json:"email"`
	Outcome    string               `json:"outcome"`
	StatusCode int                  `json:"status_code"`
	Code       string               `json:"code,omitempty"`
	Retryable  bool                 `json:"retryable,omitempty"`
	AuthStatus errorEventAuthStatus `json:"auth_status"`
}

type errorEventAuthStatus struct {
	Status         Status                 `json:"status"`
	StatusMessage  string                 `json:"status_message,omitempty"`
	Disabled       bool                   `json:"disabled"`
	Unavailable    bool                   `json:"unavailable"`
	NextRetryAfter *time.Time             `json:"next_retry_after,omitempty"`
	Quota          *errorEventQuotaStatus `json:"quota,omitempty"`
	Model          *errorEventModelStatus `json:"model,omitempty"`
}

type errorEventQuotaStatus struct {
	Exceeded      bool       `json:"exceeded"`
	Reason        string     `json:"reason,omitempty"`
	NextRecoverAt *time.Time `json:"next_recover_at,omitempty"`
	BackoffLevel  int        `json:"backoff_level,omitempty"`
}

type errorEventModelStatus struct {
	Name           string                 `json:"name"`
	Status         Status                 `json:"status"`
	StatusMessage  string                 `json:"status_message,omitempty"`
	Unavailable    bool                   `json:"unavailable"`
	NextRetryAfter *time.Time             `json:"next_retry_after,omitempty"`
	Quota          *errorEventQuotaStatus `json:"quota,omitempty"`
}

func (m *Manager) publishErrorEvent(result Result, authSnapshot *Auth) {
	if m == nil || result.Success || authSnapshot == nil || m.HomeEnabled() {
		return
	}
	payload, ok := buildErrorEventPayload(result, authSnapshot)
	if !ok {
		return
	}
	redisqueue.EnqueueError(payload)
}

func (m *Manager) publishAuthStateEvent(result Result, authSnapshot *Auth, changed bool) {
	if m == nil || m.HomeEnabled() {
		return
	}
	event, ok := buildAuthStateEvent(result, authSnapshot, changed)
	if !ok {
		return
	}
	logging.PublishFleetEvent(event)
}

func buildAuthStateEvent(result Result, authSnapshot *Auth, changed bool) (authStateEvent, bool) {
	if authSnapshot == nil || (!changed && result.Success) {
		return authStateEvent{}, false
	}
	statusCode := 200
	outcome := "recovered"
	code := ""
	retryable := false
	if !result.Success {
		statusCode = errorEventStatusCode(result.Error)
		outcome = "error"
		if result.Error != nil {
			code = strings.TrimSpace(result.Error.Code)
			retryable = result.Error.Retryable
		}
	}
	return authStateEvent{
		V:          1,
		Kind:       "proxy.auth.state",
		EventID:    uuid.NewString(),
		Timestamp:  time.Now().UTC(),
		Provider:   strings.TrimSpace(result.Provider),
		Model:      strings.TrimSpace(result.Model),
		AuthID:     strings.TrimSpace(result.AuthID),
		Account:    credSlug(result.AuthID),
		Email:      credEmail(result.AuthID),
		Outcome:    outcome,
		StatusCode: statusCode,
		Code:       code,
		Retryable:  retryable,
		AuthStatus: buildErrorEventAuthStatus(result.Model, authSnapshot),
	}, true
}

func authStateNeedsRecovery(auth *Auth, model string) bool {
	if auth == nil {
		return false
	}
	if auth.Disabled || auth.Unavailable || auth.Status != StatusActive ||
		!auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.IsZero() {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" || auth.ModelStates == nil {
		return false
	}
	state := auth.ModelStates[model]
	return state != nil && (state.Unavailable || state.Status != StatusActive ||
		!state.NextRetryAfter.IsZero() || state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() ||
		state.LastError != nil)
}

func buildErrorEventPayload(result Result, authSnapshot *Auth) ([]byte, bool) {
	if authSnapshot == nil || result.Success {
		return nil, false
	}
	authSnapshot.EnsureIndex()
	event := errorEvent{
		Timestamp:  time.Now(),
		Provider:   strings.TrimSpace(result.Provider),
		Model:      strings.TrimSpace(result.Model),
		AuthID:     strings.TrimSpace(result.AuthID),
		AuthIndex:  strings.TrimSpace(authSnapshot.Index),
		StatusCode: errorEventStatusCode(result.Error),
		Body:       errorEventBody(result.Error),
		AuthStatus: buildErrorEventAuthStatus(result.Model, authSnapshot),
	}
	if result.Error != nil {
		event.Code = strings.TrimSpace(result.Error.Code)
		event.Retryable = result.Error.Retryable
	}
	payload, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		return nil, false
	}
	return payload, true
}

func buildErrorEventAuthStatus(model string, authSnapshot *Auth) errorEventAuthStatus {
	status := errorEventAuthStatus{
		Status:         authSnapshot.Status,
		StatusMessage:  strings.TrimSpace(authSnapshot.StatusMessage),
		Disabled:       authSnapshot.Disabled,
		Unavailable:    authSnapshot.Unavailable,
		NextRetryAfter: timePtrIfSet(authSnapshot.NextRetryAfter),
		Quota:          errorEventQuotaStatusFrom(authSnapshot.Quota),
	}
	if modelState := errorEventModelStatusFrom(model, authSnapshot); modelState != nil {
		status.Model = modelState
	}
	return status
}

func errorEventModelStatusFrom(model string, authSnapshot *Auth) *errorEventModelStatus {
	model = strings.TrimSpace(model)
	if model == "" || authSnapshot == nil || authSnapshot.ModelStates == nil {
		return nil
	}
	state := authSnapshot.ModelStates[model]
	if state == nil {
		return nil
	}
	return &errorEventModelStatus{
		Name:           model,
		Status:         state.Status,
		StatusMessage:  strings.TrimSpace(state.StatusMessage),
		Unavailable:    state.Unavailable,
		NextRetryAfter: timePtrIfSet(state.NextRetryAfter),
		Quota:          errorEventQuotaStatusFrom(state.Quota),
	}
}

func errorEventQuotaStatusFrom(quota QuotaState) *errorEventQuotaStatus {
	if !quota.Exceeded && strings.TrimSpace(quota.Reason) == "" && quota.NextRecoverAt.IsZero() && quota.BackoffLevel == 0 {
		return nil
	}
	return &errorEventQuotaStatus{
		Exceeded:      quota.Exceeded,
		Reason:        strings.TrimSpace(quota.Reason),
		NextRecoverAt: timePtrIfSet(quota.NextRecoverAt),
		BackoffLevel:  quota.BackoffLevel,
	}
}

func errorEventStatusCode(err *Error) int {
	if err != nil && err.HTTPStatus > 0 {
		return err.HTTPStatus
	}
	return 500
}

func errorEventBody(err *Error) string {
	if err == nil {
		return "request failed"
	}
	if msg := strings.TrimSpace(err.Message); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(err.Error()); msg != "" {
		return msg
	}
	return "request failed"
}

func timePtrIfSet(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copyValue := value
	return &copyValue
}
