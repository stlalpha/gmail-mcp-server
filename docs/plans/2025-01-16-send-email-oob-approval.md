# Design: Email Sending with OOB Approval

**Date:** 2025-01-16
**Status:** Ready for implementation
**Pattern:** Agent Cut-Out (see `docs/agent-cut-out-pattern.md`)

## Overview

Implement email sending capability with an out-of-band (OOB) approval channel that prevents agent manipulation of the approval flow.

## User Flow

1. User starts MCP server → server prints OOB dashboard URL
2. User opens `http://localhost:8787/outbox/{session_id}` in browser
3. User works with agent, agent creates drafts via `create_draft`
4. Agent calls `send_draft(draft_id)` → call blocks
5. Browser shows pending email with full content
6. User clicks Approve/Reject in browser
7. If approved: server sends via Gmail API
8. Server returns result to agent's blocked call

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         MCP SERVER                              │
│  ┌─────────────┐    ┌─────────────┐    ┌─────────────────────┐ │
│  │   MCP       │    │   Approval  │    │   OOB Web Server    │ │
│  │   Tools     │───→│   Queue     │←──→│   (localhost:8787)  │ │
│  │             │    │             │    │                     │ │
│  │ send_draft()│    │ - pending   │    │ GET  /outbox/{sid}  │ │
│  │             │    │ - approved  │    │ POST /approve/{id}  │ │
│  │             │    │ - rejected  │    │ POST /reject/{id}   │ │
│  │             │    │ - timeout   │    │ SSE  /events/{sid}  │ │
│  └─────────────┘    └─────────────┘    └─────────────────────┘ │
│         │                  │                     ↑              │
│         │                  │                     │              │
│         ▼                  ▼                     │              │
│  ┌─────────────┐    ┌─────────────┐              │              │
│  │   Gmail     │    │   Session   │              │              │
│  │   API       │    │   Store     │──────────────┘              │
│  │             │    │             │                             │
│  │ Drafts.Send │    │ - session ID│                             │
│  │             │    │ - pending   │                             │
│  └─────────────┘    └─────────────┘                             │
└─────────────────────────────────────────────────────────────────┘
```

## Components

### 1. Session Manager

```go
type Session struct {
    ID        string    // Crypto-random session ID
    CreatedAt time.Time
    Pending   *PendingEmail // Only one at a time
}

type PendingEmail struct {
    DraftID   string
    To        string
    Subject   string
    Body      string
    QueuedAt  time.Time
    ResultCh  chan ApprovalResult // Blocks send_draft call
}

type ApprovalResult struct {
    Approved bool
    Error    error
}
```

### 2. OOB Web Server

Runs on configurable port (default 8787), separate from any HTTP MCP transport.

**Endpoints:**

| Method | Path | Description |
|--------|------|-------------|
| GET | `/outbox/{session_id}` | Dashboard HTML page |
| GET | `/api/pending/{session_id}` | Get pending email JSON |
| POST | `/api/approve/{session_id}` | Approve pending email |
| POST | `/api/reject/{session_id}` | Reject pending email |
| GET | `/events/{session_id}` | SSE stream for real-time updates |

### 3. MCP Tool: `send_draft`

```go
// Tool definition
mcp.NewTool("send_draft",
    mcp.WithDescription("Submit a draft for user approval and sending. "+
        "This call BLOCKS until the user approves or rejects via the OOB dashboard. "+
        "The agent cannot see or influence the approval process."),
    mcp.WithString("draft_id",
        mcp.Required(),
        mcp.Description("The Gmail draft ID to send"),
    ),
)
```

**Behavior:**
1. Validate draft exists and belongs to user
2. Fetch full draft content from Gmail API
3. Check no other email is pending (reject if so)
4. Queue email in session's pending slot
5. Block on `ResultCh`
6. On approval: call `Drafts.Send()`, return success
7. On rejection: return rejection status
8. On timeout (5 min): return timeout error

### 4. Dashboard UI

Simple, clean HTML page with:
- Auto-refresh via SSE (or polling fallback)
- Display: To, Subject, Full Body (rendered safely)
- Two buttons: Approve (green), Reject (red)
- Status indicator: "Waiting for email..." / "Pending approval"
- History of sent/rejected emails in session (optional)

**Security:**
- Session ID in URL is crypto-random (32 bytes, base64url)
- No authentication beyond URL knowledge
- Same-origin only (localhost)
- CSP headers to prevent injection

## Constraints Enforced

| Constraint | Implementation |
|------------|----------------|
| One pending at a time | `Session.Pending` is single pointer, not slice |
| Request timeout | Goroutine with 5-minute timer on `ResultCh` |
| No approval caching | Each `send_draft` call creates new pending entry |
| Session isolation | 256-bit random session ID |
| Server-side execution | `Drafts.Send()` called in server, not exposed to agent |

## Error Cases

| Scenario | Response to Agent |
|----------|-------------------|
| Draft doesn't exist | Error: "Draft not found" |
| Another email pending | Error: "Another email is pending approval" |
| User rejects | `{status: "rejected", message: "User rejected send"}` |
| Timeout (5 min) | Error: "Approval timed out" |
| Gmail API error | Error: "Failed to send: {details}" |
| Session expired | Error: "Session expired, restart server" |

## Startup Flow

```
$ gmail-mcp-server
📁 App data directory: ~/.auto-gmail
🔑 Token file: ~/.auto-gmail/token.json
📝 Style guide: ~/.auto-gmail/personal-email-style-guide.md

🌐 OOB Approval Dashboard:
   http://localhost:8787/outbox/x7k2m9p4q1w8e5r2t6y3u0i9

   Open this URL in your browser to approve outgoing emails.
   Keep this tab open while working with your agent.

✅ Server ready! Waiting for MCP client connections...
```

## File Changes

| File | Changes |
|------|---------|
| `main.go` | Add OOB server, session manager, `send_draft` tool |
| `go.mod` | No new dependencies needed (stdlib has everything) |

## Implementation Steps

1. Add session manager with pending email queue
2. Add OOB HTTP server with endpoints
3. Add `send_draft` MCP tool with blocking behavior
4. Add dashboard HTML template
5. Add SSE endpoint for real-time updates
6. Wire up Gmail `Drafts.Send()` on approval
7. Add startup message with dashboard URL
8. Test full flow end-to-end

## Testing

- [ ] Manual: Create draft → send_draft → approve in browser → verify sent
- [ ] Manual: Create draft → send_draft → reject in browser → verify not sent
- [ ] Manual: Create draft → send_draft → wait 5 min → verify timeout
- [ ] Manual: Two send_draft calls → verify second is rejected
- [ ] Manual: Invalid draft ID → verify error

## Future Enhancements

- Session persistence across server restarts
- Email history in dashboard
- Undo send (cancel within N seconds)
- Multiple email preview before batch approval
- Desktop notifications on pending email
