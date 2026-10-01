// Package simsig authenticates notifications (callbacks and webhooks) sent
// by the provider simulator. The scheme is the simulator's own; a real
// provider's scheme is implemented in that provider's adapter.
//
// Scheme: header X-Sim-Timestamp carries Unix seconds; header
// X-Sim-Signature carries hex(HMAC-SHA256(secret, "<timestamp>.<body>")).
// Signing the timestamp together with the body is what makes a captured
// notification useless outside the tolerance window.
package simsig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"bankplatform.internal/services/lending/app"
)

var (
	ErrBadSignature = fmt.Errorf("%w: signature is invalid", app.ErrCallbackRejected)
	ErrStale        = fmt.Errorf("%w: timestamp is outside the accepted window", app.ErrCallbackRejected)
)

// Tolerance is how far a notification's timestamp may differ from now. It
// bounds how long a captured notification could be replayed; deduplication
// by event id covers replays inside the window.
const Tolerance = 5 * time.Minute

// Authenticate checks the signature over the raw body in constant time and
// then the timestamp window. The body must not be parsed before it returns
// nil.
func Authenticate(secret string, h http.Header, body []byte, now time.Time) error {
	timestamp := h.Get("X-Sim-Timestamp")
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	got, err := hex.DecodeString(h.Get("X-Sim-Signature"))
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Tolerance || d < -Tolerance {
		return ErrStale
	}
	return nil
}
