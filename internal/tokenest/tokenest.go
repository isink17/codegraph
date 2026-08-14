// Package tokenest is CodeGraph's one deterministic token estimator.
//
// It is an ESTIMATE, not a model provider's tokenizer: no vocabulary, no BPE
// merges, no network, no external dependency. The contract is deliberately
// trivial and stable so that a token budget means the same thing in every tool,
// in every test, and across releases:
//
//	estimated_tokens = ceil(serialized_UTF8_bytes / 4)
//
// Four bytes per token is the usual rule of thumb for English prose and source
// code alike. Rounding up means a non-empty payload never estimates as zero
// tokens, which is what a budgeting loop needs to make progress.
package tokenest

import "encoding/json"

// BytesPerToken is the divisor of the estimator contract.
const BytesPerToken = 4

// FromBytes converts a serialized byte count to estimated tokens, rounding up.
// A negative count is treated as zero.
func FromBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + BytesPerToken - 1) / BytesPerToken
}

// OfString estimates the tokens of an already-serialized payload.
func OfString(s string) int { return FromBytes(len(s)) }

// OfJSON marshals v as compact JSON and returns its estimated tokens together
// with the exact serialized byte count. Budget code measures what it will
// actually emit rather than guessing field sizes, so this returns both numbers
// from one marshal.
func OfJSON(v any) (tokens, bytes int, err error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return 0, 0, err
	}
	return FromBytes(len(payload)), len(payload), nil
}
