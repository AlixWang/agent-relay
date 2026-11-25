package adsense

import (
	"encoding/json"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

// AdSense write placeholders (design v5 §5.3). These are intentionally not
// returned by Tools() and therefore never exposed on /mcp/adsense or /mcp.
// If code ever calls Handle with one of these names it gets not_implemented
// and no upstream request is made.
//
// Why: the running ADC only carries adsense.readonly; writes require the full
// https://www.googleapis.com/auth/adsense scope, and adunits.create/patch also
// require an AdSense for Platforms project. Enabling these before both
// conditions are met would produce a tool that always fails upstream.

// PlaceholderTools documents the five unimplemented upstream methods for the
// admin tools page/tests. Risk is high for destructive deletes, low otherwise.
func PlaceholderTools() []mcpgateway.Tool {
	return []mcpgateway.Tool{
		{
			Name: WriteCreateAdUnit, Description: "Placeholder: upstream adunits.create (not implemented; needs full adsense scope + AFP).",
			Risk: mcpgateway.RiskHigh, OpenWorld: true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
		},
		{
			Name: WritePatchAdUnit, Description: "Placeholder: upstream adunits.patch (not implemented; needs full adsense scope + AFP).",
			Risk: mcpgateway.RiskHigh, OpenWorld: true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
		},
		{
			Name: WriteCreateCustomChannel, Description: "Placeholder: upstream customchannels.create (not implemented; needs full adsense scope).",
			Risk: mcpgateway.RiskHigh, OpenWorld: true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
		},
		{
			Name: WriteDeleteCustomChannel, Description: "Placeholder: upstream customchannels.delete (not implemented; needs full adsense scope).",
			Risk: mcpgateway.RiskHigh, Destructive: true, OpenWorld: true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
		},
		{
			Name: WritePatchCustomChannel, Description: "Placeholder: upstream customchannels.patch (not implemented; needs full adsense scope).",
			Risk: mcpgateway.RiskHigh, OpenWorld: true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
		},
	}
}
