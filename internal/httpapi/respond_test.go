package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/log"
)

// An error's text can carry request input — a path parameter comes back inside
// the lookup's not-found error — so a newline in it must not start a second log
// line that Pando appears to have written.
func TestErrorLogsRequestInputOnOneLine(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(log.Into(r.Context(), zap.New(core)))

	Error(httptest.NewRecorder(), r, errors.New("no source src_1\nlevel=info msg=forged"))

	entries := logs.All()
	require.NotEmpty(t, entries)
	for _, e := range entries {
		for k, v := range e.ContextMap() {
			if s, ok := v.(string); ok {
				require.False(t, strings.ContainsAny(s, "\r\n"), "field %s carries a line break: %q", k, s)
			}
		}
	}
}
