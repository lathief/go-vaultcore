package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransferHandlerRequiresBearerToken(t *testing.T) {
	handler := &TransferHandler{Token: "secret-token"}
	for _, authorization := range []string{"", "Basic secret-token", "Bearer wrong-token"} {
		req := httptest.NewRequest(http.MethodPost, "/transfers", strings.NewReader("{}"))
		req.Header.Set("Authorization", authorization)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("authorization %q: status = %d, want %d", authorization, rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("authorization %q: WWW-Authenticate = %q, want Bearer", authorization, got)
		}
	}
}

func TestTransferHandlerRejectsTrailingJSON(t *testing.T) {
	handler := &TransferHandler{Token: "secret-token"}
	req := httptest.NewRequest(http.MethodPost, "/transfers",
		strings.NewReader(`{"source_account_id":1,"destination_account_id":2,"amount_cents":1} {}`))
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
