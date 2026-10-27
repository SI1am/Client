package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"besu-client/internal/cbdc"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestSubmitLegacyTransfer(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var sent int
	var node *httptest.Server
	node = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "eth_estimateGas":
			result = "0x5208"
		case "eth_getTransactionCount":
			result = "0x4"
		case "eth_sendRawTransaction":
			sent++
			var params []string
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Error(err)
				return
			}
			raw, err := hexutil.Decode(params[0])
			if err != nil {
				t.Error(err)
				return
			}
			tx := new(types.Transaction)
			if err := tx.UnmarshalBinary(raw); err != nil {
				t.Error(err)
				return
			}
			if tx.Type() != 0 || tx.GasPrice().Sign() != 0 || tx.Nonce() != 4 {
				t.Errorf("tx type %d gasPrice %s nonce %d", tx.Type(), tx.GasPrice(), tx.Nonce())
			}
			signer := types.NewEIP155Signer(big.NewInt(1337))
			sender, err := types.Sender(signer, tx)
			if err != nil {
				t.Error(err)
				return
			}
			if sender != crypto.PubkeyToAddress(key.PublicKey) {
				t.Errorf("sender %s", sender.Hex())
			}
			if *tx.To() != common.HexToAddress("0x1111111111111111111111111111111111111111") {
				t.Errorf("to %s", tx.To().Hex())
			}
			result = tx.Hash().Hex()
		case "eth_getTransactionReceipt":
			result = map[string]string{"status": "0x1"}
		case "eth_call":
			parsed, err := abi.JSON(bytes.NewReader(abiJSON))
			if err != nil {
				t.Error(err)
				return
			}
			packed, err := parsed.Methods["QueryMovement"].Outputs.Pack(
				"movement", "T1", "TRANSFER", "B1", "B2", "hash", "SETTLED", "2026-10-23T00:00:00Z", "txid",
			)
			if err != nil {
				t.Error(err)
				return
			}
			result = hexutil.Encode(packed)
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer node.Close()

	client, err := New(Config{
		RPC:        node.URL,
		ChainID:    1337,
		Contract:   "0x1111111111111111111111111111111111111111",
		PrivateKey: hexutil.Encode(crypto.FromECDSA(key)),
		MSP:        "Org1MSP",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, movement, err := client.Transfer(context.Background(), "B1", "B2", "T1", 250000, "0123456789abcdef", true)
	if err != nil {
		t.Fatal(err)
	}
	if !tx.Submitted || sent != 1 || movement.FromWallet != "B1" || movement.ToWallet != "B2" {
		t.Fatalf("tx %+v movement %+v sent %d", tx, movement, sent)
	}

	signed, _, err := client.Transfer(context.Background(), "B1", "B2", "T2", 1, "0123456789abcdef", false)
	if err != nil {
		t.Fatal(err)
	}
	if signed.Submitted || signed.Raw == "" || sent != 1 {
		t.Fatalf("signed tx was broadcast: %+v sent %d", signed, sent)
	}
}

func TestEstimateRevert(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	stringType, err := abi.NewType("string", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := abi.Arguments{{Type: stringType}}.Pack("ACCESS_DENIED: caller MSP Org1MSP is not authorised (allowed: CentralBankMSP)")
	if err != nil {
		t.Fatal(err)
	}
	data := hexutil.Encode(append([]byte{0x08, 0xc3, 0x79, 0xa0}, packed...))
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID any `json:"id"`
		}
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]any{
				"code":    3,
				"message": "execution reverted: ACCESS_DENIED: caller MSP Org1MSP is not authorised (allowed: CentralBankMSP)",
				"data":    data,
			},
		})
	}))
	defer node.Close()

	client, err := New(Config{
		RPC:        node.URL,
		ChainID:    1337,
		Contract:   "0x1111111111111111111111111111111111111111",
		PrivateKey: hexutil.Encode(crypto.FromECDSA(key)),
		MSP:        "Org1MSP",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Mint(context.Background(), "B1", "M1", 5, "0123456789abcdef", true)
	got, ok := err.(*cbdc.Error)
	if !ok {
		t.Fatalf("got %T %v", err, err)
	}
	if got.Code != "ACCESS_DENIED" {
		t.Fatalf("code %s text %s", got.Code, got.Text)
	}
}

func TestBalanceDecode(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := abi.JSON(bytes.NewReader(abiJSON))
	if err != nil {
		t.Fatal(err)
	}
	packed, err := parsed.Methods["CheckBalance"].Outputs.Pack(struct {
		WalletId       string
		Settled        *big.Int
		PendingCredits *big.Int
		Available      *big.Int
	}{"B1", big.NewInt(0), big.NewInt(1000000), big.NewInt(1000000)})
	if err != nil {
		t.Fatal(err)
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID any `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  hexutil.Encode(packed),
		})
	}))
	defer node.Close()
	client, err := New(Config{
		RPC:        node.URL,
		ChainID:    1337,
		Contract:   "0x1111111111111111111111111111111111111111",
		PrivateKey: hexutil.Encode(crypto.FromECDSA(key)),
		MSP:        "Org1MSP",
	})
	if err != nil {
		t.Fatal(err)
	}
	balance, err := client.Balance(context.Background(), "B1")
	if err != nil {
		t.Fatal(err)
	}
	if balance.Settled != 0 || balance.PendingCredits != 1000000 || balance.Available != 1000000 {
		t.Fatalf("%+v", balance)
	}
}
