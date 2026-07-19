package main

import (
	"fmt"
	"os"
	"stashcli/stashgram"
)

var cfg stashgram.Settings = stashgram.Settings{}

func main() {

	/* Load Config */
	if er := stashgram.LoadJSON("settings.json", &cfg); er != nil {
		fmt.Println(er)
		os.Exit(0)
	}

	if er := rootCmd.Execute(); er != nil {
		fmt.Println(er)
		os.Exit(0)
	}
}
