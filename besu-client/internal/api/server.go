package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"besu-client/internal/cbdc"
)

// Service is the Besu-backed contract used by the HTTP handlers.
type Service interface {
	Ready(ctx context.Context) error
	Identity() cbdc.Identity
	OnChainMSP(ctx context.Context) (string, error)
	CreateWallet(ctx context.Context, id, owner, kind string, submit bool) (cbdc.Tx, cbdc.Wallet, error)
	FreezeWallet(ctx context.Context, id, reason string, submit bool) (cbdc.Tx, cbdc.Wallet, error)
	UnfreezeWallet(ctx context.Context, id string, submit bool) (cbdc.Tx, cbdc.Wallet, error)
	Mint(ctx context.Context, walletID, movementID string, amount int64, salt string, submit bool) (cbdc.Tx, cbdc.Movement, error)
	Transfer(ctx context.Context, fromID, toID, movementID string, amount int64, salt string, submit bool) (cbdc.Tx, cbdc.Movement, error)
	Redeem(ctx context.Context, walletID, movementID string, amount int64, salt string, submit bool) (cbdc.Tx, cbdc.Movement, error)
	Consolidate(ctx context.Context, walletID string, submit bool) (cbdc.Tx, int64, error)
	Balance(ctx context.Context, walletID string) (cbdc.Balance, error)
	Wallet(ctx context.Context, id string) (cbdc.Wallet, error)
	Movement(ctx context.Context, id string) (cbdc.Movement, error)
	Verify(ctx context.Context, id string, amount int64, salt string) (bool, error)
	Supply(ctx context.Context) (cbdc.Supply, error)
	Enroll(ctx context.Context, account, mspID string, submit bool) (cbdc.Tx, cbdc.Enrollment, error)
}

// New returns the REST handler. A non-empty token requires Authorization: Bearer.
func New(svc Service, token string) http.Handler {
	h := &handler{svc: svc, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("POST /api/v1/participants", h.enroll)
	mux.HandleFunc("POST /api/v1/wallets", h.createWallet)
	mux.HandleFunc("POST /api/v1/wallets/{id}/freeze", h.freeze)
	mux.HandleFunc("POST /api/v1/wallets/{id}/unfreeze", h.unfreeze)
	mux.HandleFunc("POST /api/v1/wallets/{id}/consolidate", h.consolidate)
	mux.HandleFunc("GET /api/v1/wallets/{id}/balance", h.balance)
	mux.HandleFunc("GET /api/v1/wallets/{id}", h.wallet)
	mux.HandleFunc("POST /api/v1/movements/mint", h.mint)
	mux.HandleFunc("POST /api/v1/movements/transfer", h.transfer)
	mux.HandleFunc("POST /api/v1/movements/redeem", h.redeem)
	mux.HandleFunc("POST /api/v1/movements/{id}/verify", h.verify)
	mux.HandleFunc("GET /api/v1/movements/{id}", h.movement)
	mux.HandleFunc("GET /api/v1/supply", h.supply)
	return h.guard(mux)
}

type handler struct {
	svc   Service
	token string
}

type response struct {
	TxID           string `json:"txId,omitempty"`
	Status         string `json:"status"`
	Payload        any    `json:"payload,omitempty"`
	RawTransaction string `json:"rawTransaction,omitempty"`
}

func (h *handler) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !h.authorized(r) {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or wrong API token", "")
				return
			}
			caller := r.Header.Get("X-Caller-MSP")
			if caller != "" && caller != h.svc.Identity().MSP {
				writeError(w, http.StatusForbidden, "ACCESS_DENIED", "this client acts as "+h.svc.Identity().MSP, "")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (h *handler) authorized(r *http.Request) bool {
	if h.token == "" {
		return true
	}
	header := r.Header.Get("Authorization")
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	id := h.svc.Identity()
	body := map[string]any{
		"status":   "ok",
		"mode":     "besu",
		"msp":      id.MSP,
		"address":  id.Address,
		"contract": id.Contract,
		"ready":    true,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.svc.Ready(ctx); err != nil {
		body["ready"] = false
		body["readyError"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	msp, err := h.svc.OnChainMSP(ctx)
	if err == nil {
		body["enrolled"] = true
		body["onChainMsp"] = msp
	} else {
		body["enrolled"] = false
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *handler) enroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Address string `json:"address"`
		MSPID   string `json:"mspId"`
	}
	if !decode(w, r, &body) {
		return
	}
	h.mutate(w, r, func(ctx context.Context, submit bool) (cbdc.Tx, any, error) {
		tx, enrollment, err := h.svc.Enroll(ctx, body.Address, body.MSPID, submit)
		return tx, enrollment, err
	})
}

func (h *handler) createWallet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WalletID        string `json:"walletId"`
		OwnerMSP        string `json:"ownerMsp"`
		ParticipantType string `json:"participantType"`
	}
	if !decode(w, r, &body) {
		return
	}
	h.mutate(w, r, func(ctx context.Context, submit bool) (cbdc.Tx, any, error) {
		tx, wallet, err := h.svc.CreateWallet(ctx, body.WalletID, body.OwnerMSP, body.ParticipantType, submit)
		return tx, wallet, err
	})
}

func (h *handler) freeze(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	h.mutate(w, r, func(ctx context.Context, submit bool) (cbdc.Tx, any, error) {
		tx, wallet, err := h.svc.FreezeWallet(ctx, id, body.Reason, submit)
		return tx, wallet, err
	})
}

func (h *handler) unfreeze(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.mutate(w, r, func(ctx context.Context, submit bool) (cbdc.Tx, any, error) {
		tx, wallet, err := h.svc.UnfreezeWallet(ctx, id, submit)
		return tx, wallet, err
	})
}

func (h *handler) consolidate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.mutate(w, r, func(ctx context.Context, submit bool) (cbdc.Tx, any, error) {
		tx, settled, err := h.svc.Consolidate(ctx, id, submit)
		if err != nil || !submit {
			return tx, nil, err
		}
		return tx, map[string]any{"walletId": id, "consolidatedBalance": settled}, nil
	})
}

func (h *handler) balance(w http.ResponseWriter, r *http.Request) {
	h.read(w, r, func(ctx context.Context) (any, error) {
		return h.svc.Balance(ctx, r.PathValue("id"))
	})
}

func (h *handler) wallet(w http.ResponseWriter, r *http.Request) {
	h.read(w, r, func(ctx context.Context) (any, error) {
		return h.svc.Wallet(ctx, r.PathValue("id"))
	})
}

func (h *handler) mint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WalletID   string `json:"walletId"`
		MovementID string `json:"movementId"`
		Amount     amount `json:"amount"`
		Salt       string `json:"salt"`
	}
	if !decode(w, r, &body) {
		return
	}
	h.mutate(w, r, func(ctx context.Context, submit bool) (cbdc.Tx, any, error) {
		tx, movement, err := h.svc.Mint(ctx, body.WalletID, body.MovementID, int64(body.Amount), body.Salt, submit)
		return tx, movement, err
	})
}

func (h *handler) transfer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FromWalletID string `json:"fromWalletId"`
		ToWalletID   string `json:"toWalletId"`
		MovementID   string `json:"movementId"`
		Amount       amount `json:"amount"`
		Salt         string `json:"salt"`
	}
	if !decode(w, r, &body) {
		return
	}
	h.mutate(w, r, func(ctx context.Context, submit bool) (cbdc.Tx, any, error) {
		tx, movement, err := h.svc.Transfer(ctx, body.FromWalletID, body.ToWalletID, body.MovementID, int64(body.Amount), body.Salt, submit)
		return tx, movement, err
	})
}

func (h *handler) redeem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WalletID   string `json:"walletId"`
		MovementID string `json:"movementId"`
		Amount     amount `json:"amount"`
		Salt       string `json:"salt"`
	}
	if !decode(w, r, &body) {
		return
	}
	h.mutate(w, r, func(ctx context.Context, submit bool) (cbdc.Tx, any, error) {
		tx, movement, err := h.svc.Redeem(ctx, body.WalletID, body.MovementID, int64(body.Amount), body.Salt, submit)
		return tx, movement, err
	})
}

func (h *handler) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Amount amount `json:"amount"`
		Salt   string `json:"salt"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	h.read(w, r, func(ctx context.Context) (any, error) {
		ok, err := h.svc.Verify(ctx, id, int64(body.Amount), body.Salt)
		if err != nil {
			return nil, err
		}
		return map[string]any{"movementId": id, "verified": ok}, nil
	})
}

func (h *handler) movement(w http.ResponseWriter, r *http.Request) {
	h.read(w, r, func(ctx context.Context) (any, error) {
		return h.svc.Movement(ctx, r.PathValue("id"))
	})
}

func (h *handler) supply(w http.ResponseWriter, r *http.Request) {
	h.read(w, r, func(ctx context.Context) (any, error) {
		return h.svc.Supply(ctx)
	})
}

func (h *handler) mutate(w http.ResponseWriter, r *http.Request, run func(context.Context, bool) (cbdc.Tx, any, error)) {
	submit := r.URL.Query().Get("submit") != "false"
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	tx, payload, err := run(ctx, submit)
	if err != nil {
		fail(w, err)
		return
	}
	status := "COMMITTED"
	raw := ""
	if !tx.Submitted {
		status = "SIGNED"
		raw = tx.Raw
		payload = nil
	}
	writeJSON(w, http.StatusOK, response{TxID: tx.Hash, Status: status, Payload: payload, RawTransaction: raw})
}

func (h *handler) read(w http.ResponseWriter, r *http.Request, run func(context.Context) (any, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	payload, err := run(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response{Status: "QUERIED", Payload: payload})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), "")
		return false
	}
	return true
}

func fail(w http.ResponseWriter, err error) {
	var ce *cbdc.Error
	if errors.As(err, &ce) {
		writeError(w, statusFor(ce.Code), ce.Code, ce.Text, ce.TxID)
		return
	}
	writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error(), "")
}

func statusFor(code string) int {
	switch code {
	case "INVALID_ARGUMENT":
		return http.StatusBadRequest
	case "UNAUTHORIZED":
		return http.StatusUnauthorized
	case "ACCESS_DENIED":
		return http.StatusForbidden
	case "NOT_FOUND":
		return http.StatusNotFound
	case "ALREADY_EXISTS", "DUPLICATE_REQUEST":
		return http.StatusConflict
	case "INSUFFICIENT_BALANCE", "ACCOUNT_BLOCKED", "INVALID_STATE":
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, status int, code, text, txID string) {
	body := map[string]string{"error": text, "code": code}
	if txID != "" {
		body["txId"] = txID
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// amount accepts a JSON integer or a decimal string. Floats are rejected
// so a value above 2^53 cannot be rounded.
type amount int64

func (a *amount) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return errors.New("amount is required")
	}
	if text[0] == '"' {
		unquoted, err := strconv.Unquote(text)
		if err != nil {
			return err
		}
		text = unquoted
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return errors.New("amount must be an integer")
	}
	*a = amount(n)
	return nil
}
