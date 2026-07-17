package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestModelExecutionHeadersExplicitEmptyIsAuthoritative(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("Session-Id", "context-session")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	got := modelExecutionHeaders(ctx, http.Header{})
	if got == nil || len(got) != 0 {
		t.Fatalf("explicit empty headers = %#v, want non-nil empty clone", got)
	}

	fallback := modelExecutionHeaders(ctx, nil)
	if fallback.Get("Session-Id") != "context-session" {
		t.Fatalf("nil headers fallback = %#v, want context headers", fallback)
	}
}
