package stashgram

import (
	"errors"
	"math/rand"
	"regexp"
	"strconv"
	"time"
)

// Retry knobs for chunk upload/download. These are the main defense against
// bad/censored networks: DPI-based connection resets, timeouts, and
// mid-transfer drops all show up as ordinary errors here, and get retried
// with backoff instead of failing the whole transfer.
const (
	MaxRetries     = 8
	RetryBaseDelay = 2 * time.Second
	RetryMaxDelay  = 60 * time.Second
)

var floodWaitRe = regexp.MustCompile(`FLOOD_WAIT_(\d+)`)

// floodWaitSeconds extracts Telegram's requested wait time, if the error is
// a FLOOD_WAIT_n from MTProto.
func floodWaitSeconds(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	m := floodWaitRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0, false
	}
	return n, true
}

// withRetry retries fn with exponential backoff + jitter. FLOOD_WAIT_n
// errors are honored exactly (sleep n seconds) rather than guessed at.
func withRetry(op string, fn func() error) error {
	var lastErr error
	delay := RetryBaseDelay

	for attempt := 1; attempt <= MaxRetries; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		if secs, ok := floodWaitSeconds(err); ok {
			time.Sleep(time.Duration(secs)*time.Second + time.Second)
			continue
		}

		if attempt == MaxRetries {
			break
		}
		jitter := time.Duration(rand.Int63n(int64(delay)/2 + 1))
		time.Sleep(delay + jitter)
		delay *= 2
		if delay > RetryMaxDelay {
			delay = RetryMaxDelay
		}
	}
	return errors.New(op + ": giving up after " + strconv.Itoa(MaxRetries) + " attempts: " + lastErr.Error())
}
