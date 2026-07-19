package main

import (
	"fmt"
	"os"
	"path/filepath"
	"stashcli/stashgram"
)

var cfg = stashgram.Settings{}

func main() {
	// Get the directory of the executable
	exePath, err := os.Executable()
	if err != nil {
		fmt.Println("Failed to get executable path:", err)
		os.Exit(1)
	}
	configPath := filepath.Join(filepath.Dir(exePath), "settings.json")

	if er := stashgram.LoadJSON(configPath, &cfg); er != nil {
		fmt.Println(er)
		os.Exit(0)
	}

	if er := rootCmd.Execute(); er != nil {
		fmt.Println(er)
		os.Exit(0)
	}
}
