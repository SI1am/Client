// Command besu-client is the REST front door for WholesaleCBDC on Besu.
// One process holds one organization's key. It signs and submits.
// The four Besu validators run the contract and finalize the block.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"besu-client/internal/api"
	"besu-client/internal/chain"
)

func main() {
	listen := flag.String("listen", env("BESU_LISTEN", ":8080"), "HTTP listen address")
	rpc := flag.String("rpc", env("BESU_RPC", "http://127.0.0.1:8545"), "Besu JSON-RPC URL")
	chainID := flag.Int64("chain-id", envInt("BESU_CHAIN_ID", 1337), "chain id")
	contract := flag.String("contract", os.Getenv("BESU_CONTRACT"), "WholesaleCBDC address")
	key := flag.String("key", os.Getenv("BESU_PRIVATE_KEY"), "hex private key for this organization")
	msp := flag.String("msp", os.Getenv("BESU_MSP"), "MSP id this process acts as")
	token := flag.String("token", os.Getenv("BESU_API_TOKEN"), "optional bearer token for /api")
	flag.Parse()

	if *contract == "" || *key == "" || *msp == "" {
		fmt.Fprintln(os.Stderr, "contract, key, and msp are required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	client, err := chain.Dial(ctx, chain.Config{
		RPC:        *rpc,
		ChainID:    *chainID,
		Contract:   *contract,
		PrivateKey: *key,
		MSP:        *msp,
	})
	cancel()
	if err != nil {
		log.Fatal(err)
	}

	id := client.Identity()
	server := &http.Server{
		Addr:              *listen,
		Handler:           api.New(client, *token),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("besu-client %s at %s listening on %s, contract %s", id.MSP, id.Address, *listen, id.Contract)
	log.Fatal(server.ListenAndServe())
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int64) int64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}
