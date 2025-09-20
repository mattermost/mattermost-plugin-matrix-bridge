package test

import (
	"testing"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/pkg/errors"
)

func TestIsRateLimitError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name: "Matrix 429 error with M_LIMIT_EXCEEDED",
			err: &matrix.Error{
				StatusCode: 429,
				ErrCode:    "M_LIMIT_EXCEEDED",
				ErrMsg:     "Too Many Requests",
			},
			expected: true,
		},
		{
			name: "Matrix error with M_LIMIT_EXCEEDED code only",
			err: &matrix.Error{
				StatusCode: 500,
				ErrCode:    "M_LIMIT_EXCEEDED",
				ErrMsg:     "Rate limit exceeded",
			},
			expected: true,
		},
		{
			name: "Matrix 429 error without M_LIMIT_EXCEEDED",
			err: &matrix.Error{
				StatusCode: 429,
				ErrCode:    "UNKNOWN",
				ErrMsg:     "Too Many Requests",
			},
			expected: true,
		},
		{
			name: "Non-rate limit Matrix error",
			err: &matrix.Error{
				StatusCode: 400,
				ErrCode:    "M_INVALID_PARAM",
				ErrMsg:     "Bad request",
			},
			expected: false,
		},
		{
			name:     "Non-Matrix error",
			err:      errors.New("network error"),
			expected: false,
		},
		{
			name:     "Nil error",
			err:      nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matrix.IsRateLimitError(tt.err)
			if result != tt.expected {
				t.Errorf("IsRateLimitError() = %v, expected %v for error: %v", result, tt.expected, tt.err)
			}
		})
	}
}
