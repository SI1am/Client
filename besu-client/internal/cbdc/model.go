package cbdc

import "strings"

// Error is a WholesaleCBDC revert. Code is the prefix before ": ",
// such as ACCESS_DENIED. Text is the rest of the message.
type Error struct {
	Code string
	Text string
	TxID string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Text
	}
	if e.Text == "" {
		return e.Code
	}
	return e.Code + ": " + e.Text
}

// ParseRevert splits a contract or client message into a code and text.
func ParseRevert(message string) *Error {
	message = strings.TrimSpace(message)
	for _, prefix := range []string{
		"execution reverted: ",
		"VM Exception while processing transaction: revert ",
		"revert ",
	} {
		message = strings.TrimPrefix(message, prefix)
	}
	code, text, ok := strings.Cut(message, ": ")
	if !ok || code == "" || strings.Contains(code, " ") {
		return &Error{Code: "INTERNAL_ERROR", Text: message}
	}
	return &Error{Code: code, Text: text}
}

// Identity is the organization this process signs for.
type Identity struct {
	MSP      string
	Address  string
	Contract string
}

// Tx is a signed Besu transaction. Submitted is false when the caller
// asked not to broadcast it.
type Tx struct {
	Hash      string
	Raw       string
	Submitted bool
}

// Wallet is the public wallet record returned by QueryWallet.
type Wallet struct {
	DocType         string `json:"docType"`
	WalletID        string `json:"walletId"`
	OwnerMSP        string `json:"ownerMsp"`
	ParticipantType string `json:"participantType"`
	Status          string `json:"status"`
	FreezeReason    string `json:"freezeReason"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

// Movement is the public movement record. It carries the amount hash,
// not the amount.
type Movement struct {
	DocType      string `json:"docType"`
	MovementID   string `json:"movementId"`
	MovementType string `json:"movementType"`
	FromWallet   string `json:"fromWallet"`
	ToWallet     string `json:"toWallet"`
	AmountHash   string `json:"amountHash"`
	Status       string `json:"status"`
	Timestamp    string `json:"timestamp"`
	TxID         string `json:"txId"`
}

// Balance is settled funds plus credits that are still pending.
type Balance struct {
	WalletID       string `json:"walletId"`
	Settled        int64  `json:"settled"`
	PendingCredits int64  `json:"pendingCredits"`
	Available      int64  `json:"available"`
}

// Supply is the central-bank view of coins issued and not yet redeemed.
type Supply struct {
	TotalSupply int64  `json:"totalSupply"`
	Entries     uint64 `json:"entries"`
}

// Enrollment is the result of binding an address to an MSP id.
type Enrollment struct {
	Address  string `json:"address"`
	MSPID    string `json:"mspId"`
	Enrolled bool   `json:"enrolled"`
}
