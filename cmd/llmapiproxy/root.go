package main

import (
	"os"

	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags.
var version = "dev"

var rootCmd = &cobra.Command{
	Version: version,
	Use:   "llmapiproxy",
	Short: "LLM API Proxy — unified OpenAI-compatible endpoint for multiple providers",
	Long: `LLM API Proxy is a reverse proxy that unifies multiple LLM provider APIs
(OpenRouter, Z.ai, OpenCode Zen/Go, etc.) behind a single OpenAI-compatible endpoint.
It includes a web dashboard, request stats tracking, quota monitoring, and a chat playground.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(userCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
