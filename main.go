package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"stashcli/stashgram"
)

func defaultSettings() stashgram.Settings {
	return stashgram.Settings{
		UploadChunkSize:            471859200,
		APIID:                      2040,
		APIHASH:                    "b18441a1ff607e10a989891a5462e627",
		ParralDownload:             4,
		ParralUpload:               4,
		Proxy:                      nil, // or &stashgram.ProxyConfig{...}
		CacheMaxSizeMB:             200,
		CacheExpireDays:            7,
		UploadBandwidthLimitKBps:   0,
		DownloadBandwidthLimitKBps: 0,
		OperationTimeoutSec:        0,
		KeepAliveIntervalSec:       0,
		StreamAddr:                 "127.0.0.1",
		StreamPort:                 8081,
		StreamUser:                 "",
		StreamPass:                 "",
	}
}

var cfg stashgram.Settings

func main() {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Println("Failed to get executable path:", err)
		os.Exit(1)
	}
	configPath := filepath.Join(filepath.Dir(exePath), "settings.json")

	err = stashgram.LoadJSON(configPath, &cfg)
	if err != nil {
		if os.IsNotExist(err) {
			cfg = defaultSettings()
			data, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				fmt.Println("Failed to marshal default settings:", err)
				os.Exit(1)
			}

			if err := os.WriteFile(configPath, data, 0644); err != nil {
				fmt.Println("Failed to write settings.json:", err)
				os.Exit(1)
			}
		} else {
			fmt.Println("Failed to load settings.json:", err)
			os.Exit(1)
		}
	}

	// Execute the root command (Cobra)
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(0)
	}
}
