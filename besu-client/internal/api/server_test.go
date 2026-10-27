package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"besu-client/internal/cbdc"
)

func TestTransferAndCallerGuard(t *testing.T) {
	fake := &fakeService{msp: "Org1MSP"}
	srv := httptest.NewServer(New(fake, ""))
	defer srv.Close()

	body := []byte(`{"fromWalletId":"B1","toWalletId":"B2","movementId":"T1","amount":250000,"salt":"0123456789abcdef"}`)
	res, err := http.Post(srv.URL+"/api/v1/movements/transfer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var got struct {
		TxID    string `json:"txId"`
		Status  string `json:"status"`
		Payload struct {
			MovementID string `json:"movementId"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "COMMITTED" || got.TxID != "0xabc" || got.Payload.MovementID != "T1" {
		t.Fatalf("response %+v", got)
	}
	if !fake.transferSubmit {
		t.Fatal("transfer was not broadcast")
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/movements/transfer?submit=false", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caller-MSP", "CentralBankMSP")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("caller status %d", res.StatusCode)
	}
}

func TestRevertStatus(t *testing.T) {
	fake := &fakeService{msp: "Org1MSP", err: &cbdc.Error{Code: "INSUFFICIENT_BALANCE", Text: "wallet B1 has insufficient CBDC"}}
	srv := httptest.NewServer(New(fake, "secret"))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/movements/redeem", bytes.NewReader([]byte(`{"walletId":"B1","movementId":"R1","amount":"10","salt":"0123456789abcdef"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", res.StatusCode)
	}
	var got struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Code != "INSUFFICIENT_BALANCE" || got.Error != "wallet B1 has insufficient CBDC" {
		t.Fatalf("error %+v", got)
	}
}

func TestBalanceQuery(t *testing.T) {
	fake := &fakeService{msp: "Org1MSP"}
	srv := httptest.NewServer(New(fake, ""))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/v1/wallets/B1/balance")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got struct {
		Status  string       `json:"status"`
		Payload cbdc.Balance `json:"payload"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "QUERIED" || got.Payload.Available != 1000000 {
		t.Fatalf("balance %+v", got)
	}
}

type fakeService struct {
	msp            string
	err            error
	transferSubmit bool
}

func (f *fakeService) Ready(context.Context) error { return nil }
func (f *fakeService) Identity() cbdc.Identity {
	return cbdc.Identity{MSP: f.msp, Address: "0xabc", Contract: "0xdef"}
}
func (f *fakeService) OnChainMSP(context.Context) (string, error) { return f.msp, nil }
func (f *fakeService) CreateWallet(context.Context, string, string, string, bool) (cbdc.Tx, cbdc.Wallet, error) {
	return cbdc.Tx{}, cbdc.Wallet{}, f.err
}
func (f *fakeService) FreezeWallet(context.Context, string, string, bool) (cbdc.Tx, cbdc.Wallet, error) {
	return cbdc.Tx{}, cbdc.Wallet{}, f.err
}
func (f *fakeService) UnfreezeWallet(context.Context, string, bool) (cbdc.Tx, cbdc.Wallet, error) {
	return cbdc.Tx{}, cbdc.Wallet{}, f.err
}
func (f *fakeService) Mint(context.Context, string, string, int64, string, bool) (cbdc.Tx, cbdc.Movement, error) {
	return cbdc.Tx{}, cbdc.Movement{}, f.err
}
func (f *fakeService) Transfer(_ context.Context, _, _, movement string, _ int64, _ string, submit bool) (cbdc.Tx, cbdc.Movement, error) {
	f.transferSubmit = submit
	if f.err != nil {
		return cbdc.Tx{}, cbdc.Movement{}, f.err
	}
	tx := cbdc.Tx{Hash: "0xabc", Raw: "0xraw", Submitted: submit}
	return tx, cbdc.Movement{MovementID: movement}, nil
}
func (f *fakeService) Redeem(context.Context, string, string, int64, string, bool) (cbdc.Tx, cbdc.Movement, error) {
	return cbdc.Tx{}, cbdc.Movement{}, f.err
}
func (f *fakeService) Consolidate(context.Context, string, bool) (cbdc.Tx, int64, error) {
	return cbdc.Tx{}, 0, f.err
}
func (f *fakeService) Balance(context.Context, string) (cbdc.Balance, error) {
	return cbdc.Balance{WalletID: "B1", Settled: 0, PendingCredits: 1000000, Available: 1000000}, f.err
}
func (f *fakeService) Wallet(context.Context, string) (cbdc.Wallet, error) {
	return cbdc.Wallet{}, f.err
}
func (f *fakeService) Movement(context.Context, string) (cbdc.Movement, error) {
	return cbdc.Movement{}, f.err
}
func (f *fakeService) Verify(context.Context, string, int64, string) (bool, error) {
	return true, f.err
}
func (f *fakeService) Supply(context.Context) (cbdc.Supply, error) {
	return cbdc.Supply{}, f.err
}
func (f *fakeService) Enroll(context.Context, string, string, bool) (cbdc.Tx, cbdc.Enrollment, error) {
	return cbdc.Tx{}, cbdc.Enrollment{}, f.err
}
