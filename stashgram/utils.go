package stashgram

import (
	"encoding/json"
	"fmt"
	"os"
)

func LoadJSON(path string, destObj any) error {
	content, er := os.ReadFile(path)
	if er != nil {
		return er
	}

	er = json.Unmarshal(content, destObj)
	return er
}

func SaveJSON(path string, srcObj any) error {
	content, er := json.Marshal(srcObj)
	if er != nil {
		return er
	}

	er = os.WriteFile(path, content, 0666)
	return er
}

// HumanSize formats a byte count the way people expect to read it (e.g.
// "450.00 MiB" instead of "471859200"), used by `stashcli info` and `ls`.
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
