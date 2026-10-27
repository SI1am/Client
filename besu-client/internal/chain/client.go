package chain

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"besu-client/internal/cbdc"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

//go:embed WholesaleCBDC.abi
var abiJSON []byte

const revertSelector = "08c379a0"

// Config is the connection for one organization.
type Config struct {
	RPC         string
	ChainID     int64
	Contract    string
	PrivateKey  string
	MSP         string
	ReceiptWait time.Duration
}

// Client signs WholesaleCBDC transactions for one key and reads the contract back.
type Client struct {
	rpcURL      string
	chainID     *big.Int
	contract    common.Address
	key         *ecdsa.PrivateKey
	address     common.Address
	msp         string
	abi         abi.ABI
	http        *http.Client
	receiptWait time.Duration
	mu          sync.Mutex
	nextID      atomic.Uint64
}

// New parses the key and the contract ABI. It does not contact the node.
func New(cfg Config) (*Client, error) {
	if cfg.RPC == "" {
		return nil, fmt.Errorf("rpc url is required")
	}
	if cfg.ChainID <= 0 {
		return nil, fmt.Errorf("chain id is required")
	}
	if !common.IsHexAddress(cfg.Contract) {
		return nil, fmt.Errorf("contract address is invalid")
	}
	if strings.TrimSpace(cfg.MSP) == "" {
		return nil, fmt.Errorf("msp id is required")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(cfg.PrivateKey), "0x"))
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	parsed, err := abi.JSON(bytes.NewReader(abiJSON))
	if err != nil {
		return nil, fmt.Errorf("abi: %w", err)
	}
	wait := cfg.ReceiptWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	return &Client{
		rpcURL:      cfg.RPC,
		chainID:     big.NewInt(cfg.ChainID),
		contract:    common.HexToAddress(cfg.Contract),
		key:         key,
		address:     crypto.PubkeyToAddress(key.PublicKey),
		msp:         cfg.MSP,
		abi:         parsed,
		http:        &http.Client{Timeout: 20 * time.Second},
		receiptWait: wait,
	}, nil
}

// Dial connects and checks that this chain id has the contract deployed.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	client, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if err := client.CheckDeployment(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

// CheckDeployment confirms the node is the expected chain and the address has code.
func (c *Client) CheckDeployment(ctx context.Context) error {
	raw, err := c.rpc(ctx, "eth_chainId", []any{})
	if err != nil {
		return err
	}
	var hexID string
	if err := json.Unmarshal(raw, &hexID); err != nil {
		return fmt.Errorf("chain id: %w", err)
	}
	got, err := parseHexBig(hexID)
	if err != nil {
		return fmt.Errorf("chain id: %w", err)
	}
	if got.Cmp(c.chainID) != 0 {
		return fmt.Errorf("node chain id is %s, this client is configured for %s", got, c.chainID)
	}
	raw, err = c.rpc(ctx, "eth_getCode", []any{c.contract.Hex(), "latest"})
	if err != nil {
		return err
	}
	var code string
	if err := json.Unmarshal(raw, &code); err != nil {
		return fmt.Errorf("contract code: %w", err)
	}
	if code == "" || code == "0x" || code == "0x0" {
		return fmt.Errorf("no contract code at %s", c.contract.Hex())
	}
	return nil
}

func (c *Client) Identity() cbdc.Identity {
	return cbdc.Identity{MSP: c.msp, Address: c.address.Hex(), Contract: c.contract.Hex()}
}

func (c *Client) Ready(ctx context.Context) error {
	_, err := c.rpc(ctx, "eth_blockNumber", []any{})
	return err
}

func (c *Client) OnChainMSP(ctx context.Context) (string, error) {
	vals, err := c.call(ctx, "CallerMSP")
	if err != nil {
		return "", err
	}
	return asString(vals, 0), nil
}

func (c *Client) CreateWallet(ctx context.Context, id, owner, kind string, submit bool) (cbdc.Tx, cbdc.Wallet, error) {
	tx, err := c.submit(ctx, "CreateWallet", submit, id, owner, kind)
	if err != nil || !submit {
		return tx, cbdc.Wallet{}, err
	}
	wallet, err := c.Wallet(ctx, id)
	if err != nil {
		return tx, cbdc.Wallet{WalletID: id}, nil
	}
	return tx, wallet, nil
}

func (c *Client) FreezeWallet(ctx context.Context, id, reason string, submit bool) (cbdc.Tx, cbdc.Wallet, error) {
	tx, err := c.submit(ctx, "FreezeWallet", submit, id, reason)
	if err != nil || !submit {
		return tx, cbdc.Wallet{}, err
	}
	wallet, err := c.Wallet(ctx, id)
	if err != nil {
		return tx, cbdc.Wallet{WalletID: id}, nil
	}
	return tx, wallet, nil
}

func (c *Client) UnfreezeWallet(ctx context.Context, id string, submit bool) (cbdc.Tx, cbdc.Wallet, error) {
	tx, err := c.submit(ctx, "UnfreezeWallet", submit, id)
	if err != nil || !submit {
		return tx, cbdc.Wallet{}, err
	}
	wallet, err := c.Wallet(ctx, id)
	if err != nil {
		return tx, cbdc.Wallet{WalletID: id}, nil
	}
	return tx, wallet, nil
}

func (c *Client) Mint(ctx context.Context, walletID, movementID string, amount int64, salt string, submit bool) (cbdc.Tx, cbdc.Movement, error) {
	tx, err := c.submit(ctx, "MintCBDC", submit, walletID, movementID, big.NewInt(amount), salt)
	if err != nil || !submit {
		return tx, cbdc.Movement{}, err
	}
	movement, err := c.Movement(ctx, movementID)
	if err != nil {
		return tx, cbdc.Movement{MovementID: movementID}, nil
	}
	return tx, movement, nil
}

func (c *Client) Transfer(ctx context.Context, fromID, toID, movementID string, amount int64, salt string, submit bool) (cbdc.Tx, cbdc.Movement, error) {
	tx, err := c.submit(ctx, "TransferCBDC", submit, fromID, toID, movementID, big.NewInt(amount), salt)
	if err != nil || !submit {
		return tx, cbdc.Movement{}, err
	}
	movement, err := c.Movement(ctx, movementID)
	if err != nil {
		return tx, cbdc.Movement{MovementID: movementID}, nil
	}
	return tx, movement, nil
}

func (c *Client) Redeem(ctx context.Context, walletID, movementID string, amount int64, salt string, submit bool) (cbdc.Tx, cbdc.Movement, error) {
	tx, err := c.submit(ctx, "RedeemCBDC", submit, walletID, movementID, big.NewInt(amount), salt)
	if err != nil || !submit {
		return tx, cbdc.Movement{}, err
	}
	movement, err := c.Movement(ctx, movementID)
	if err != nil {
		return tx, cbdc.Movement{MovementID: movementID}, nil
	}
	return tx, movement, nil
}

func (c *Client) Consolidate(ctx context.Context, walletID string, submit bool) (cbdc.Tx, int64, error) {
	tx, err := c.submit(ctx, "ConsolidateBalance", submit, walletID)
	if err != nil || !submit {
		return tx, 0, err
	}
	balance, err := c.Balance(ctx, walletID)
	if err != nil {
		return tx, 0, nil
	}
	return tx, balance.Settled, nil
}

func (c *Client) Balance(ctx context.Context, walletID string) (cbdc.Balance, error) {
	vals, err := c.call(ctx, "CheckBalance", walletID)
	if err != nil {
		return cbdc.Balance{}, err
	}
	view, err := convert[balanceView](vals)
	if err != nil {
		return cbdc.Balance{}, err
	}
	settled, err := int64From(view.Settled)
	if err != nil {
		return cbdc.Balance{}, err
	}
	pending, err := int64From(view.PendingCredits)
	if err != nil {
		return cbdc.Balance{}, err
	}
	available, err := int64From(view.Available)
	if err != nil {
		return cbdc.Balance{}, err
	}
	return cbdc.Balance{WalletID: view.WalletId, Settled: settled, PendingCredits: pending, Available: available}, nil
}

func (c *Client) Wallet(ctx context.Context, id string) (cbdc.Wallet, error) {
	vals, err := c.call(ctx, "QueryWallet", id)
	if err != nil {
		return cbdc.Wallet{}, err
	}
	if len(vals) != 8 {
		return cbdc.Wallet{}, fmt.Errorf("QueryWallet returned %d values", len(vals))
	}
	return cbdc.Wallet{
		DocType:         asString(vals, 0),
		WalletID:        asString(vals, 1),
		OwnerMSP:        asString(vals, 2),
		ParticipantType: asString(vals, 3),
		Status:          asString(vals, 4),
		FreezeReason:    asString(vals, 5),
		CreatedAt:       asString(vals, 6),
		UpdatedAt:       asString(vals, 7),
	}, nil
}

func (c *Client) Movement(ctx context.Context, id string) (cbdc.Movement, error) {
	vals, err := c.call(ctx, "QueryMovement", id)
	if err != nil {
		return cbdc.Movement{}, err
	}
	if len(vals) != 9 {
		return cbdc.Movement{}, fmt.Errorf("QueryMovement returned %d values", len(vals))
	}
	return cbdc.Movement{
		DocType:      asString(vals, 0),
		MovementID:   asString(vals, 1),
		MovementType: asString(vals, 2),
		FromWallet:   asString(vals, 3),
		ToWallet:     asString(vals, 4),
		AmountHash:   asString(vals, 5),
		Status:       asString(vals, 6),
		Timestamp:    asString(vals, 7),
		TxID:         asString(vals, 8),
	}, nil
}

func (c *Client) Verify(ctx context.Context, id string, amount int64, salt string) (bool, error) {
	vals, err := c.call(ctx, "VerifyMovementAmount", id, big.NewInt(amount), salt)
	if err != nil {
		return false, err
	}
	if len(vals) != 1 {
		return false, fmt.Errorf("VerifyMovementAmount returned %d values", len(vals))
	}
	ok, isBool := vals[0].(bool)
	if !isBool {
		return false, fmt.Errorf("VerifyMovementAmount returned %T", vals[0])
	}
	return ok, nil
}

func (c *Client) Supply(ctx context.Context) (cbdc.Supply, error) {
	vals, err := c.call(ctx, "QueryTotalSupply")
	if err != nil {
		return cbdc.Supply{}, err
	}
	view, err := convert[supplyView](vals)
	if err != nil {
		return cbdc.Supply{}, err
	}
	total, err := int64From(view.TotalSupply)
	if err != nil {
		return cbdc.Supply{}, err
	}
	if view.Entries == nil || !view.Entries.IsUint64() {
		return cbdc.Supply{}, fmt.Errorf("supply entries do not fit uint64")
	}
	return cbdc.Supply{TotalSupply: total, Entries: view.Entries.Uint64()}, nil
}

func (c *Client) Enroll(ctx context.Context, account, mspID string, submit bool) (cbdc.Tx, cbdc.Enrollment, error) {
	if !common.IsHexAddress(account) {
		return cbdc.Tx{}, cbdc.Enrollment{}, &cbdc.Error{Code: "INVALID_ARGUMENT", Text: "address is invalid"}
	}
	addr := common.HexToAddress(account)
	tx, err := c.submit(ctx, "EnrollParticipant", submit, addr, mspID)
	if err != nil || !submit {
		return tx, cbdc.Enrollment{}, err
	}
	return tx, cbdc.Enrollment{Address: addr.Hex(), MSPID: mspID, Enrolled: true}, nil
}

type balanceView struct {
	WalletId       string
	Settled        *big.Int
	PendingCredits *big.Int
	Available      *big.Int
}

type supplyView struct {
	TotalSupply *big.Int
	Entries     *big.Int
}

func (c *Client) call(ctx context.Context, method string, args ...any) ([]any, error) {
	data, err := c.abi.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", method, err)
	}
	raw, err := c.rpc(ctx, "eth_call", []any{c.callObj(data), "latest"})
	if err != nil {
		return nil, asContract(err)
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, fmt.Errorf("decode %s result: %w", method, err)
	}
	out, err := hexutil.Decode(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode %s result: %w", method, err)
	}
	vals, err := c.abi.Methods[method].Outputs.Unpack(out)
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", method, err)
	}
	return vals, nil
}

func (c *Client) submit(ctx context.Context, method string, broadcast bool, args ...any) (cbdc.Tx, error) {
	data, err := c.abi.Pack(method, args...)
	if err != nil {
		return cbdc.Tx{}, fmt.Errorf("pack %s: %w", method, err)
	}
	gas, err := c.estimateGas(ctx, data)
	if err != nil {
		return cbdc.Tx{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	nonce, err := c.pendingNonce(ctx)
	if err != nil {
		return cbdc.Tx{}, err
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      gas + 100_000,
		To:       &c.contract,
		Data:     data,
	})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(c.chainID), c.key)
	if err != nil {
		return cbdc.Tx{}, fmt.Errorf("sign %s: %w", method, err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return cbdc.Tx{}, err
	}
	out := cbdc.Tx{Hash: signed.Hash().Hex(), Raw: hexutil.Encode(raw)}
	if !broadcast {
		return out, nil
	}
	if _, err := c.rpc(ctx, "eth_sendRawTransaction", []any{out.Raw}); err != nil {
		return cbdc.Tx{}, asContract(err)
	}
	if err := c.wait(ctx, signed.Hash(), data); err != nil {
		return cbdc.Tx{}, err
	}
	out.Submitted = true
	return out, nil
}

func (c *Client) estimateGas(ctx context.Context, data []byte) (uint64, error) {
	raw, err := c.rpc(ctx, "eth_estimateGas", []any{c.callObj(data)})
	if err != nil {
		return 0, asContract(err)
	}
	var hexGas string
	if err := json.Unmarshal(raw, &hexGas); err != nil {
		return 0, err
	}
	return parseHexUint(hexGas)
}

func (c *Client) pendingNonce(ctx context.Context) (uint64, error) {
	raw, err := c.rpc(ctx, "eth_getTransactionCount", []any{c.address.Hex(), "pending"})
	if err != nil {
		return 0, err
	}
	var hexNonce string
	if err := json.Unmarshal(raw, &hexNonce); err != nil {
		return 0, err
	}
	return parseHexUint(hexNonce)
}

func (c *Client) wait(ctx context.Context, hash common.Hash, data []byte) error {
	deadline := time.Now().Add(c.receiptWait)
	for {
		raw, err := c.rpc(ctx, "eth_getTransactionReceipt", []any{hash.Hex()})
		if err != nil {
			return err
		}
		if string(raw) != "null" {
			var rec struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			if rec.Status == "0x1" {
				return nil
			}
			reason := c.replay(ctx, data)
			if ce, ok := reason.(*cbdc.Error); ok {
				ce.TxID = hash.Hex()
			}
			return reason
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for receipt %s", hash.Hex())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (c *Client) replay(ctx context.Context, data []byte) error {
	_, err := c.rpc(ctx, "eth_call", []any{c.callObj(data), "latest"})
	if err == nil {
		return &cbdc.Error{Code: "INTERNAL_ERROR", Text: "transaction reverted"}
	}
	return asContract(err)
}

func (c *Client) callObj(data []byte) map[string]string {
	return map[string]string{
		"from": c.address.Hex(),
		"to":   c.contract.Hex(),
		"data": hexutil.Encode(data),
	}
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

type rpcFailure struct {
	Message string
	Data    string
}

func (e *rpcFailure) Error() string { return e.Message }

func (c *Client) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID.Add(1),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpcURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var decoded rpcResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("%s: %s", method, bytes.TrimSpace(payload))
	}
	if decoded.Error != nil {
		return nil, &rpcFailure{Message: decoded.Error.Message, Data: dataString(decoded.Error.Data)}
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", method, res.StatusCode)
	}
	return decoded.Result, nil
}

func dataString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var obj struct {
		Data    string `json:"data"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Data != "" {
			return obj.Data
		}
		return obj.Message
	}
	return ""
}

func asContract(err error) error {
	var failure *rpcFailure
	if !errorsAs(err, &failure) {
		return err
	}
	if failure.Data != "" {
		if text, ok := decodeRevert(failure.Data); ok {
			return cbdc.ParseRevert(text)
		}
	}
	return cbdc.ParseRevert(failure.Message)
}

func errorsAs(err error, target **rpcFailure) bool {
	for err != nil {
		if failure, ok := err.(*rpcFailure); ok {
			*target = failure
			return true
		}
		unwrap, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrap.Unwrap()
	}
	return false
}

func decodeRevert(data string) (string, bool) {
	raw := strings.TrimPrefix(data, "0x")
	if len(raw) < 8 || !strings.EqualFold(raw[:8], revertSelector) {
		return "", false
	}
	body, err := hexutil.Decode("0x" + raw[8:])
	if err != nil {
		return "", false
	}
	stringType, err := abi.NewType("string", "", nil)
	if err != nil {
		return "", false
	}
	vals, err := abi.Arguments{{Type: stringType}}.Unpack(body)
	if err != nil || len(vals) != 1 {
		return "", false
	}
	text, ok := vals[0].(string)
	return text, ok
}

func parseHexUint(value string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 64)
}

func parseHexBig(value string) (*big.Int, error) {
	n := new(big.Int)
	_, ok := n.SetString(strings.TrimPrefix(value, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("invalid hex %q", value)
	}
	return n, nil
}

func asString(vals []any, index int) string {
	if index >= len(vals) || vals[index] == nil {
		return ""
	}
	text, ok := vals[index].(string)
	if !ok {
		return fmt.Sprint(vals[index])
	}
	return text
}

func asInt64(vals []any, index int) (int64, error) {
	if index >= len(vals) {
		return 0, fmt.Errorf("missing return value %d", index)
	}
	n, ok := vals[index].(*big.Int)
	if !ok {
		return 0, fmt.Errorf("return value %d is %T", index, vals[index])
	}
	return int64From(n)
}

func int64From(n *big.Int) (int64, error) {
	if n == nil || !n.IsInt64() {
		return 0, fmt.Errorf("value does not fit int64")
	}
	return n.Int64(), nil
}

func convert[T any](vals []any) (T, error) {
	var zero T
	if len(vals) != 1 {
		return zero, fmt.Errorf("expected one return value, got %d", len(vals))
	}
	converted := abi.ConvertType(vals[0], new(T))
	out, ok := converted.(*T)
	if !ok || out == nil {
		return zero, fmt.Errorf("cannot decode %T", vals[0])
	}
	return *out, nil
}
