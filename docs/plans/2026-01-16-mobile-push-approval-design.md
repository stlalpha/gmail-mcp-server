# Mobile Push Approval System Design

## Problem

The current TOTP-based Agent Cut-Out implementation has a critical flaw: if the TOTP secret is stored anywhere the agent can access (files, environment, logs), the agent could generate valid codes and approve its own emails.

## Solution

Replace TOTP with mobile push notifications via ntfy.sh, running in a separate daemon process that the agent cannot access.

## Architecture

```
┌─────────────────┐     Unix Socket     ┌──────────────────────┐
│  Gmail MCP      │◄──────────────────►│  Approval Daemon      │
│  Server         │  "queue this email" │  (separate process)   │
└─────────────────┘                     └──────────┬───────────┘
        │                                          │
        │                                          │ HTTPS
        ▼                                          ▼
   Gmail API                                  ntfy.sh
                                                   │
                                                   │ Push
                                                   ▼
                                            ┌──────────────┐
                                            │  User's      │
                                            │  Phone       │
                                            │  (ntfy app)  │
                                            └──────────────┘
```

### Why Two Processes?

The agent runs in the same context as the MCP server. If the MCP server held the ntfy topic secret, the agent could potentially send fake approvals. The separate daemon creates a true security boundary - the agent has no way to reach it.

## Components

### 1. Gmail MCP Server (modified)

- Handles all Gmail operations (search, read, draft)
- When sending: creates draft, asks Approval Daemon to queue it
- Communicates with daemon via Unix socket
- No longer hosts OOB dashboard

### 2. Approval Daemon (new binary)

- Runs as separate process
- Stores ntfy topic secret (agent can't access)
- Sends push notifications via ntfy.sh
- Polls ntfy.sh for approve/reject responses
- Exposes only ONE operation: "queue this email for approval"

## First-Run Setup Flow

1. **Generate secrets**
   - Random ntfy topic: `gmail-mcp-[32 random chars]`
   - Signing secret for one-time tokens
   - Save to `~/.config/gmail-mcp/approval-daemon.json`

2. **Launch setup page**
   - Start web server on random port
   - Auto-open in browser

3. **Setup page shows**
   - QR code for ntfy topic subscription
   - Manual topic string as fallback
   - "Test Notification" button
   - "Complete Setup" button (requires test success)

4. **After setup**
   - Daemon enters normal operation
   - Subsequent starts skip setup

### Re-running Setup

Delete config file or run `gmail-approval-daemon --reset`.

## Email Approval Flow

### Step 1: MCP Server → Daemon

```json
{
  "action": "queue_email",
  "to": "jim@signalmine.com",
  "subject": "URGENT: The Geese Have Organized",
  "body": "Hi Jim...",
  "draft_id": "r123456789"
}
```

### Step 2: Daemon Generates Tokens

- `approve_token`: random 32 chars, valid 5 min
- `reject_token`: random 32 chars, valid 5 min

### Step 3: Daemon Sends Notification

```
POST https://ntfy.sh/gmail-mcp-a8f3x9k2

Title: Approve email?
Body: To: jim@signalmine.com
      Subject: URGENT: The Geese Have Organized

Actions:
  - Approve button → publishes APPROVE:<token> to topic
  - Reject button → publishes REJECT:<token> to topic
```

### Step 4: Daemon Polls for Response

- Checks ntfy.sh every 1 second
- Validates token matches
- Times out after 5 minutes

### Step 5: On Approval

- Returns success to MCP server via socket
- MCP server sends draft via Gmail API

## Security Properties

| Property | Implementation |
|----------|----------------|
| Agent can't approve | Approval only via ntfy.sh with valid token |
| Agent can't read secrets | Secrets in separate daemon process |
| Agent can't forge tokens | Tokens cryptographically random, never exposed |
| No replay attacks | Tokens one-time use, 5 min expiry |
| No eavesdropping | ntfy topic is random, only user knows it |

## Edge Cases

| Scenario | Behavior |
|----------|----------|
| Timeout (5 min) | Email rejected, agent notified |
| Daemon not running | Socket error, clear error message to agent |
| Multiple pending | Only one at a time, second request rejected |
| Daemon crashes | Pending lost, agent gets timeout |
| ntfy.sh down | Retry 3x, then fail with error |
| Missed approval | Daemon polls with `since=` to catch up |

## File Locations

```
~/.config/gmail-mcp/
├── approval-daemon.json    # ntfy topic, signing secret
├── approval.sock           # Unix socket for IPC
├── token.json              # Gmail OAuth (existing)
└── style-guide.md          # Email style (existing)
```

## Commands

```bash
# Start daemon (required for send feature)
gmail-approval-daemon

# Reset and re-run setup
gmail-approval-daemon --reset

# Check status
gmail-approval-daemon --status
```

## Integration

MCP server checks for socket on startup:
- If daemon not running: `send_email_ato` returns error with instructions
- Other tools (search, read, draft) work normally

## Distribution

Two binaries from same repo:
- `gmail-mcp-server` - MCP server (existing)
- `gmail-approval-daemon` - approval daemon (new)

## Future Considerations

- Launcher script to start both processes
- systemd/launchd service definitions
- Self-hosted ntfy option for extra paranoid users
