package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/lathief/go-vaultcore/internal/transfer"
)

// errorEnvelope is the single error shape of the API.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type TransferHandler struct {
	Service *transfer.Service
	Logger  *slog.Logger
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var e errorEnvelope
	e.Error.Code, e.Error.Message = code, msg
	_ = json.NewEncoder(w).Encode(e)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *TransferHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var req transfer.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "malformed JSON body")
		return
	}

	idemKey := r.Header.Get("Idempotency-Key")
	res, replayed, err := h.Service.Execute(r.Context(), idemKey, req)
	if err != nil {
		h.handleError(w, r, idemKey, err)
		return
	}

	status := http.StatusOK
	if replayed {
		// 200 for replay vs 201 for first execution; the body is consistent.
		status = http.StatusOK
	} else {
		status = http.StatusCreated
	}
	w.Header().Set("Idempotency-Replayed", replayedString(replayed))
	writeJSON(w, status, res)
}

func replayedString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (h *TransferHandler) handleError(w http.ResponseWriter, r *http.Request, idemKey string, err error) {
	var (
		verr  *transfer.ValidationError
		cerr  *transfer.ConflictError
		rerr2 *transfer.KeyReusedError
		aerr  *transfer.AccountError
		iferr *transfer.InsufficientFundsError
	)
	switch {
	case errors.As(err, &verr):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", verr.Msg)
	case errors.As(err, &rerr2):
		writeError(w, http.StatusConflict, "idempotency_key_reused", rerr2.Error())
	case errors.As(err, &cerr):
		// If the key actually completed, replay wins over the conflict.
		if res, ok, rerr := h.Service.ReplayResult(r.Context(), idemKey); rerr == nil && ok {
			w.Header().Set("Idempotency-Replayed", "true")
			writeJSON(w, http.StatusOK, res)
			return
		}
		writeError(w, http.StatusConflict, "idempotency_conflict", cerr.Msg)
	case errors.As(err, &iferr):
		writeError(w, http.StatusUnprocessableEntity, "insufficient_funds", iferr.Error())
	case errors.As(err, &aerr):
		writeError(w, http.StatusUnprocessableEntity, "account_error", aerr.Msg)
	default:
		h.Logger.Error("transfer failed", "error", err, "idem_key", idemKey, "ts", time.Now().UTC())
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
