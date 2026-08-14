package mcp

import (
	"context"
	"encoding/json"

	"github.com/isink17/codegraph/internal/detail"
	"github.com/isink17/codegraph/internal/usage"
)

// The usage meter measures the model-visible layer of this server and nothing
// else. Two facts about where it is wired matter more than the arithmetic:
//
//   - Sizes are read from representations that already exist. A tool's content
//     text is measured with len() after it is produced; the two `tools/list`
//     payloads are marshalled once at package initialization. No response is
//     serialized a second time to be counted.
//   - Attribution happens at callToolContent, the one canonical choke point every
//     model-visible tool call passes through exactly once, whether it arrived
//     directly or through the gateway. That is what makes a forwarded call one
//     row rather than two.
//
// Everything else -- session totals, discovery cost, direct/gateway split,
// format and detail buckets -- falls out of those two facts.

// callMeter is the per-call scratch record threaded through one `tools/call`.
//
// It is created by handle, annotated by whichever layers know something the
// caller did not state, and booked once when the call is complete. It is
// mutated only by the goroutine serving its own request, so it needs no lock:
// the shared aggregate behind it is what synchronizes.
type callMeter struct {
	tool   string
	via    usage.Via
	format string
	detail string
}

// maxUsageToolRows caps what `limit` can ask for. Without it the argument added
// to bound the report would be the way to unbound it: the meter's own answer must
// never become the most expensive response in the session.
const maxUsageToolRows = 200

type callMeterKeyType struct{}

// callMeterKey is the context key for the in-flight record.
var callMeterKey callMeterKeyType

func withCallMeter(ctx context.Context, rec *callMeter) context.Context {
	return context.WithValue(ctx, callMeterKey, rec)
}

func callMeterFrom(ctx context.Context) *callMeter {
	rec, _ := ctx.Value(callMeterKey).(*callMeter)
	return rec
}

// newCallMeter opens a record for one `tools/call`.
//
// The rule for the `tool` dimension, stated once and applied everywhere: a row
// names the tool the model asked for, resolved against the canonical registry,
// with gateway forwarding replacing tool_call by its target once that target
// resolves. A name that is not in the registry is usage.UnknownTool, so a typo
// or a probe cannot become a metric label.
func newCallMeter(name string) *callMeter {
	rec := &callMeter{tool: usage.UnknownTool, via: usage.ViaDirect}
	if _, ok := toolByName[name]; ok {
		rec.tool = name
	}
	return rec
}

// markGateway records that this call arrived through tool_call. It is set on
// entry to the forwarding path, before the target is resolved, so a gateway call
// that reaches no capability is still reported as a gateway call -- charged to
// tool_call, because tool_call is what ran.
func (r *callMeter) markGateway() {
	if r != nil {
		r.via = usage.ViaGateway
	}
}

// observeTool records the canonical tool this call reached. It is set before
// argument validation, so a call rejected by the validator is still charged to
// the capability it was aimed at rather than to the path that carried it.
// A name the registry does not know leaves the record on usage.UnknownTool: an
// unrecognized string must never become a metric label.
//
// It returns the resolved descriptor so the caller can hand it to
// meterDimensions instead of resolving the same name twice on the hot path.
func (r *callMeter) observeTool(name string) *toolDescriptor {
	desc, ok := toolByName[name]
	if !ok {
		return nil
	}
	if r != nil {
		r.tool = name
	}
	return desc
}

// observeDimensions records the two enum-valued arguments worth breaking out,
// once they are known to be valid. Only tools that actually accept an argument
// get a bucket for it, so a tool without a `detail` argument is never reported
// as if it had chosen the default -- and a call that never got past validation
// carries neither, because it never chose either.
func (r *callMeter) observeDimensions(format, detailLevel string) {
	if r == nil {
		return
	}
	r.format = format
	r.detail = detailLevel
}

func (r *callMeter) key() usage.Key {
	if r == nil {
		return usage.Key{Tool: usage.UnknownTool, Via: usage.ViaDirect}
	}
	return usage.Key{Tool: r.tool, Via: r.via, Format: r.format, Detail: r.detail}
}

// meterDimensions reports the effective format and detail labels for one call.
//
// Both values arrive already decoded by the caller's single pass over the
// arguments, so metering adds no parse of its own. An omitted level resolves to
// the real effective default rather than becoming a fifth bucket, which is what
// makes "how expensive is full versus card" answerable. The "unknown" bucket is
// defensive: validateToolArguments has already rejected an invalid level by the
// time this runs, so an arbitrary string cannot reach the dimension table.
func meterDimensions(desc *toolDescriptor, format, rawDetail string) (string, string) {
	if desc == nil {
		return "", ""
	}
	formatLabel := ""
	if desc.acceptsFormat {
		formatLabel = format
	}
	detailLabel := ""
	if desc.acceptsDetail {
		level, err := detail.Parse(rawDetail, defaultToolDetail)
		if err != nil {
			detailLabel = "unknown"
		} else {
			detailLabel = string(level)
		}
	}
	return formatLabel, detailLabel
}

// handleUsageStats answers the usage_stats diagnostic.
//
// usage_stats is registered but not advertised: it is absent from both
// `tools/list` payloads, so neither the full surface nor the gateway surface
// grows by a byte for having a meter. It stays callable by exact name in either
// mode, and discoverable through tool_search in gateway mode.
//
// The returned snapshot covers usage up to, but not including, this call. This
// call's own request and response are booked by handle after the response text
// exists, and appear in the next snapshot.
func (s *Server) handleUsageStats(_ context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		Reset bool `json:"reset"`
		Limit int  `json:"limit"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	if req.Limit > maxUsageToolRows {
		req.Limit = maxUsageToolRows
	}
	return map[string]any{"ok": true, "data": s.usage.Snapshot(req.Reset, req.Limit)}, nil
}

// UsageSnapshot returns the current report without disturbing it. It exists for
// the server's own shutdown summary and for tests; nothing on the tool surface
// calls it.
func (s *Server) UsageSnapshot() usage.Report { return s.usage.Snapshot(false, 0) }
