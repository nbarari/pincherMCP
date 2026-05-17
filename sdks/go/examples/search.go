//go:build sdkexample
// +build sdkexample

// NOTE: the build tag above excludes this file from the default
// `go test ./...` walk because it imports github.com/kwad77/pincher-sdk-go
// which only exists after the user runs scripts/generate-sdks.sh AND
// wires a `replace` directive against the generated tree (per the
// instructions block below). Build the example explicitly with:
//
//   go run -tags sdkexample sdks/go/examples/search.go ProcessPayment
//
// Without the tag, CI's `go test ./...` would fail with "no required
// module provides package github.com/kwad77/pincher-sdk-go" on every
// run of every PR — even when the PR has nothing to do with the SDK.

// Minimal example: call `pincher search` from Go via the generated SDK.
// Assumes you've run `scripts/generate-sdks.sh go` against a running
// pincher. To consume the local generated SDK without publishing, add
// to your go.mod:
//
//	replace github.com/kwad77/pincher-sdk-go => ../path/to/sdks/go/generated
//	require github.com/kwad77/pincher-sdk-go v0.0.0
//
// Run with:
//
//	PINCHER_HTTP_URL=http://localhost:8080 \
//	PINCHER_HTTP_KEY=$(cat ~/.pincher-key) \
//	go run sdks/go/examples/search.go ProcessPayment
package main

import (
	"context"
	"fmt"
	"os"

	pincher "github.com/kwad77/pincher-sdk-go"
)

func main() {
	baseURL := os.Getenv("PINCHER_HTTP_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	query := "Hello"
	if len(os.Args) > 1 {
		query = os.Args[1]
	}

	cfg := pincher.NewConfiguration()
	cfg.Servers = pincher.ServerConfigurations{{URL: baseURL}}
	if apiKey := os.Getenv("PINCHER_HTTP_KEY"); apiKey != "" {
		cfg.DefaultHeader["Authorization"] = "Bearer " + apiKey
	}
	client := pincher.NewAPIClient(cfg)

	ctx := context.Background()
	resp, _, err := client.SearchAPI.Search(ctx).SearchRequest(pincher.SearchRequest{
		Query: query,
		Limit: pincher.PtrInt32(5),
	}).Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("Found %d match(es) for %s:\n", len(resp.Results), query)
	for _, r := range resp.Results {
		fmt.Printf("  %s  (%s in %s:%d)\n",
			r.GetQualifiedName(), r.GetKind(), r.GetFilePath(), r.GetStartLine())
	}
}
