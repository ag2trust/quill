package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"quill/internal/events"
)

// Error is a classified provider failure.
type Error struct{ Info events.ErrorInfo }

func (e *Error) Error() string {
	msg := "provider error: " + e.Info.Kind
	if e.Info.HTTPStatus != 0 {
		msg += fmt.Sprintf(" (http %d)", e.Info.HTTPStatus)
	}
	if e.Info.Type != "" {
		msg += ": " + e.Info.Type
	}
	if e.Info.Message != "" {
		msg += ": " + e.Info.Message
	}
	return msg
}

// Classify maps a Step error to an *Error, from HTTP status and API error type
// only. It returns nil for nil and for context cancellation or deadline errors:
// those come from the caller's context, and the caller decides what they mean.
func Classify(err error) *Error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return &Error{Info: events.ErrorInfo{Kind: "network", Message: err.Error(), Retries: maxRetries}}
	}

	info := events.ErrorInfo{
		Kind:       kindForStatus(apiErr.StatusCode),
		Type:       string(apiErr.Type()),
		Message:    apiErr.Message,
		RequestID:  apiErr.RequestID,
		HTTPStatus: apiErr.StatusCode,
		Retries:    maxRetries,
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal([]byte(apiErr.RawJSON()), &body) == nil && body.RequestID != "" {
		info.RequestID = body.RequestID
	}
	if apiErr.Response != nil {
		info.RetryAfter = apiErr.Response.Header.Get("retry-after")
	}
	return &Error{Info: info}
}

func kindForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "auth"
	case status == http.StatusPaymentRequired:
		return "billing"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status == http.StatusNotFound:
		return "model_unavailable"
	case status >= 500:
		return "overloaded"
	default:
		return "bad_request"
	}
}

// Refused is the error for a response with stop reason "refusal".
func Refused() *Error {
	return &Error{Info: events.ErrorInfo{Kind: "refused", Type: "refusal", Message: "the model refused the request"}}
}
