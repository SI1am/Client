//go:build live

package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"besu-client/internal/chain"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	liveRPC = "http://127.0.0.1:8545"
	liveKey = "0x8f2a55949038a9610f50fb23b5883af3b4ecb3c3bb792cbcefbd1542c692be63"
	liveBin = `C:\Users\localadmin\Desktop\shivam\benching\contracts\build\WholesaleCBDC.bin`
	salt    = "0123456789abcdef"
)

func TestLiveCBDC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	contract := deploy(t, ctx)
	t.Logf("deployed %s", contract.Hex())

	cb := dial(t, ctx, liveKey, "CentralBankMSP", contract)
	bankAKey, bankAAddr := newKey(t)
	bankBKey, bankBAddr := newKey(t)
	bankA := dial(t, ctx, bankAKey, "Org1MSP", contract)
	bankB := dial(t, ctx, bankBKey, "Org2MSP", contract)

	cbAPI := httptest.NewServer(New(cb, ""))
	aAPI := httptest.NewServer(New(bankA, ""))
	bAPI := httptest.NewServer(New(bankB, ""))
	defer cbAPI.Close()
	defer aAPI.Close()
	defer bAPI.Close()

	status, signed := request(t, http.MethodPost, cbAPI.URL+"/api/v1/wallets?submit=false", map[string]string{
		"walletId": "CB", "ownerMsp": "CentralBankMSP", "participantType": "central_bank",
	}, nil)
	if status != http.StatusOK || signed["status"] != "SIGNED" || !strings.HasPrefix(str(signed["rawTransaction"]), "0x") {
		t.Fatalf("sign-only create: %d %+v", status, signed)
	}

	commit(t, "enrol A", cbAPI.URL+"/api/v1/participants", map[string]string{"address": bankAAddr, "mspId": "Org1MSP"})
	commit(t, "enrol B", cbAPI.URL+"/api/v1/participants", map[string]string{"address": bankBAddr, "mspId": "Org2MSP"})
	commit(t, "wallet CB", cbAPI.URL+"/api/v1/wallets", map[string]string{
		"walletId": "CB", "ownerMsp": "CentralBankMSP", "participantType": "central_bank",
	})
	commit(t, "wallet B1", cbAPI.URL+"/api/v1/wallets", map[string]string{
		"walletId": "B1", "ownerMsp": "Org1MSP", "participantType": "bank",
	})
	commit(t, "wallet B2", cbAPI.URL+"/api/v1/wallets", map[string]string{
		"walletId": "B2", "ownerMsp": "Org2MSP", "participantType": "bank",
	})
	commit(t, "mint", cbAPI.URL+"/api/v1/movements/mint", map[string]any{
		"walletId": "B1", "movementId": "M1", "amount": 1000000, "salt": salt,
	})

	_, balance := request(t, http.MethodGet, aAPI.URL+"/api/v1/wallets/B1/balance", nil, nil)
	assertBalance(t, balance, 0, 1000000, 1000000)

	status, denied := request(t, http.MethodGet, cbAPI.URL+"/api/v1/wallets/B1/balance", nil, nil)
	if status != http.StatusForbidden || denied["code"] != "ACCESS_DENIED" {
		t.Fatalf("central bank balance read: %d %+v", status, denied)
	}

	_, folded := request(t, http.MethodPost, aAPI.URL+"/api/v1/wallets/B1/consolidate", nil, nil)
	if num(payload(t, folded)["consolidatedBalance"]) != 1000000 {
		t.Fatalf("consolidate %+v", folded)
	}

	commit(t, "transfer", aAPI.URL+"/api/v1/movements/transfer", map[string]any{
		"fromWalletId": "B1", "toWalletId": "B2", "movementId": "T1", "amount": 250000, "salt": salt,
	})
	_, balance = request(t, http.MethodGet, aAPI.URL+"/api/v1/wallets/B1/balance", nil, nil)
	assertBalance(t, balance, 750000, 0, 750000)
	_, balance = request(t, http.MethodGet, bAPI.URL+"/api/v1/wallets/B2/balance", nil, nil)
	assertBalance(t, balance, 0, 250000, 250000)

	_, folded = request(t, http.MethodPost, bAPI.URL+"/api/v1/wallets/B2/consolidate", nil, nil)
	if num(payload(t, folded)["consolidatedBalance"]) != 250000 {
		t.Fatalf("B2 consolidate %+v", folded)
	}

	commit(t, "redeem", aAPI.URL+"/api/v1/movements/redeem", map[string]any{
		"walletId": "B1", "movementId": "R1", "amount": 100000, "salt": salt,
	})
	_, balance = request(t, http.MethodGet, aAPI.URL+"/api/v1/wallets/B1/balance", nil, nil)
	assertBalance(t, balance, 650000, 0, 650000)

	_, supply := request(t, http.MethodGet, cbAPI.URL+"/api/v1/supply", nil, nil)
	gotSupply := payload(t, supply)
	if num(gotSupply["totalSupply"]) != 900000 || num(gotSupply["entries"]) != 2 {
		t.Fatalf("supply %+v", supply)
	}

	_, verified := request(t, http.MethodPost, aAPI.URL+"/api/v1/movements/T1/verify", map[string]any{"amount": 250000, "salt": salt}, nil)
	if payload(t, verified)["verified"] != true {
		t.Fatalf("verify %+v", verified)
	}
	_, verified = request(t, http.MethodPost, aAPI.URL+"/api/v1/movements/T1/verify", map[string]any{"amount": 1, "salt": salt}, nil)
	if payload(t, verified)["verified"] != false {
		t.Fatalf("bad verify %+v", verified)
	}

	commit(t, "freeze", cbAPI.URL+"/api/v1/wallets/B2/freeze", map[string]string{"reason": "review"})
	status, blocked := request(t, http.MethodPost, aAPI.URL+"/api/v1/movements/transfer", map[string]any{
		"fromWalletId": "B1", "toWalletId": "B2", "movementId": "T2", "amount": 1, "salt": salt,
	}, nil)
	if status != http.StatusUnprocessableEntity || blocked["code"] != "ACCOUNT_BLOCKED" {
		t.Fatalf("frozen transfer: %d %+v", status, blocked)
	}
	commit(t, "unfreeze", cbAPI.URL+"/api/v1/wallets/B2/unfreeze", map[string]string{})

	_, wallet := request(t, http.MethodGet, cbAPI.URL+"/api/v1/wallets/B2", nil, nil)
	if payload(t, wallet)["status"] != "ACTIVE" || payload(t, wallet)["ownerMsp"] != "Org2MSP" {
		t.Fatalf("wallet %+v", wallet)
	}

	status, wrong := request(t, http.MethodGet, aAPI.URL+"/api/v1/supply", nil, map[string]string{"X-Caller-MSP": "CentralBankMSP"})
	if status != http.StatusForbidden {
		t.Fatalf("caller header: %d %+v", status, wrong)
	}
}

func deploy(t *testing.T, ctx context.Context) common.Address {
	t.Helper()
	raw, err := os.ReadFile(liveBin)
	if err != nil {
		t.Fatal(err)
	}
	code, err := hexutil.Decode("0x" + strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	key, from := parseKey(t, liveKey)
	ec, err := ethclient.Dial(liveRPC)
	if err != nil {
		t.Fatal(err)
	}
	defer ec.Close()
	nonce, err := ec.PendingNonceAt(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	gas, err := ec.EstimateGas(ctx, ethereum.CallMsg{From: from, GasPrice: big.NewInt(0), Data: code})
	if err != nil {
		t.Fatal(err)
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(0), Gas: gas + 200_000, Data: code})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(big.NewInt(1337)), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := ec.SendTransaction(ctx, signed); err != nil {
		t.Fatal(err)
	}
	receipt, err := bind.WaitMined(ctx, ec, signed)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != 1 {
		t.Fatalf("deploy status %d", receipt.Status)
	}
	return receipt.ContractAddress
}

func dial(t *testing.T, ctx context.Context, key, msp string, contract common.Address) *chain.Client {
	t.Helper()
	client, err := chain.Dial(ctx, chain.Config{
		RPC:         liveRPC,
		ChainID:     1337,
		Contract:    contract.Hex(),
		PrivateKey:  key,
		MSP:         msp,
		ReceiptWait: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func newKey(t *testing.T) (string, string) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return hexutil.Encode(crypto.FromECDSA(key)), crypto.PubkeyToAddress(key.PublicKey).Hex()
}

func parseKey(t *testing.T, hexKey string) (*ecdsa.PrivateKey, common.Address) {
	t.Helper()
	key, err := crypto.HexToECDSA(strings.TrimPrefix(hexKey, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return key, crypto.PubkeyToAddress(key.PublicKey)
}

func request(t *testing.T, method, url string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, decoded
}

func commit(t *testing.T, name, url string, body any) {
	t.Helper()
	status, decoded := request(t, http.MethodPost, url, body, nil)
	if status != http.StatusOK || decoded["status"] != "COMMITTED" {
		t.Fatalf("%s: %d %+v", name, status, decoded)
	}
}

func payload(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	got, ok := body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing in %+v", body)
	}
	return got
}

func assertBalance(t *testing.T, body map[string]any, settled, pending, available int64) {
	t.Helper()
	if body["status"] != "QUERIED" {
		t.Fatalf("balance status %+v", body)
	}
	got := payload(t, body)
	if num(got["settled"]) != settled || num(got["pendingCredits"]) != pending || num(got["available"]) != available {
		t.Fatalf("balance got %+v, want settled %d pending %d available %d", got, settled, pending, available)
	}
}

func str(value any) string {
	text, _ := value.(string)
	return text
}

func num(value any) int64 {
	switch n := value.(type) {
	case float64:
		return int64(n)
	case json.Number:
		parsed, _ := n.Int64()
		return parsed
	default:
		return 0
	}
}
