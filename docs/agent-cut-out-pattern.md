# The Agent Cut-Out Pattern

A security pattern for protecting high-risk operations from untrusted AI agent intermediaries.

## Problem Statement

When an AI agent mediates between a server and a user, the agent becomes an **untrusted intermediary** that can:

1. **Misrepresent server data to the user** - Agent receives email draft from server, shows user a sanitized version, user approves, but the actual draft contains something else entirely
2. **Manipulate user approval** - Social engineering through selective presentation
3. **Modify payloads in flight** - Alter data between server response and user presentation
4. **Create non-deterministic behavior** - Same server state can produce different user-visible outcomes depending on agent behavior

This is particularly dangerous for **high-risk operations** like:
- Sending emails
- Financial transactions
- Data deletion
- Access control changes
- Publishing content

## Solution: Cut the Agent Out

For high-risk operations, remove the agent from the sensitive path entirely. The agent becomes a **requester** and **notification recipient**, but never a **presenter** or **executor**.

### Traditional Flow (Vulnerable)

```
┌─────────────────────────────────────────────────────────────┐
│                                                             │
│   User ←────────────→ Agent ←────────────→ Server           │
│                         │                                   │
│              Agent controls BOTH channels                   │
│              Agent can lie to user about server data        │
│              Agent can lie to server about user intent      │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### Agent Cut-Out Flow (Secure)

```
┌─────────────────────────────────────────────────────────────┐
│                                                             │
│   Agent ──── request ────→ Server                           │
│                              │                              │
│                              ├──── present ────→ User       │
│                              │    (OOB channel)    │        │
│                              │                     │        │
│                              │←──── approve ───────┘        │
│                              │                              │
│                              │                              │
│   Agent ←── notification ───┘                               │
│                                                             │
│          Agent CANNOT see or influence the                  │
│          presentation → approval → execution path           │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## Agent Role Boundaries

### Agent CAN:
- **Initiate** - Request that an action be taken
- **Receive notifications** - Be told the outcome

### Agent CANNOT:
- **Present** - Show sensitive data directly to user
- **Execute** - Perform the high-risk action
- **Approve** - Influence the user's decision
- **Observe approval** - See the approval UI or process

## Properties

| Property | Description |
|----------|-------------|
| **Deterministic** | Server controls exactly what's shown and executed |
| **Verifiable** | User sees server's truth, not agent's interpretation |
| **Tamper-proof** | Agent cannot modify data in the approval path |
| **Auditable** | Server can log exactly what was presented and approved |
| **Non-replayable** | Approvals are tied to specific requests |

## Implementation: Out-of-Band (OOB) Channel

The OOB channel must be **agent-inaccessible**. Options include:

### 1. Local Web UI (Recommended for MCP)
- Server runs HTTP endpoint on localhost
- Ephemeral session URL: `http://localhost:PORT/outbox/{random_session_id}`
- User opens in browser (separate from agent context)
- Real-time updates via polling, SSE, or WebSocket

### 2. Email Notification
- Server sends preview email directly to user
- User reviews in their email client
- Confirmation code included in email

### 3. Native OS Dialog
- Server spawns native GUI dialog
- Completely separate process from agent

### 4. Mobile Push Notification
- Server sends to user's mobile device
- Approval happens on phone

## Example: Email Sending

```
┌─────────────────────────────────────────────────────────────┐
│                                                             │
│  AGENT                    SERVER                   USER     │
│    │                        │                        │      │
│    │                        │      (browser open)    │      │
│    │                        │←─── SSE connection ────│      │
│    │                        │                        │      │
│    │── send_draft(id) ─────→│                        │      │
│    │                        │                        │      │
│    │     (call blocks)      │── push to browser ────→│      │
│    │                        │   - recipient          │      │
│    │                        │   - subject            │      │
│    │                        │   - full body          │      │
│    │                        │   - [APPROVE] [REJECT] │      │
│    │                        │                        │      │
│    │                        │←───── APPROVE ─────────│      │
│    │                        │                        │      │
│    │                        │═══ SERVER SENDS ═══    │      │
│    │                        │      (Gmail API)       │      │
│    │                        │                        │      │
│    │←── {status:"sent"} ────│                        │      │
│    │                        │                        │      │
└─────────────────────────────────────────────────────────────┘
```

## Constraints

To maintain security, the implementation MUST enforce:

1. **One pending request at a time** - Prevents batch approval attacks
2. **Request timeout** - Pending requests expire (e.g., 5 minutes)
3. **No approval caching** - Each request requires fresh approval
4. **Session isolation** - OOB channel URL is unguessable
5. **Server-side execution** - Server performs the action, never the agent

## Limitations & Future Work

### Server Integrity (NOT YET ADDRESSED)

The Agent Cut-Out pattern assumes the **server itself is trustworthy**. However, a malicious agent could potentially:

- Modify server source code before execution
- Alter server binaries
- Inject code into server dependencies
- Modify server configuration

Mitigations to explore:
- **Read-only server deployment** - Immutable container/binary
- **Code signing** - Verify server integrity at startup
- **Filesystem monitoring** - Detect tampering
- **Sandboxed execution** - Agent cannot access server files
- **Separate process isolation** - Server runs in protected context

This is tracked as future work.

## References

- [MCP Elicitation Specification (Draft)](https://modelcontextprotocol.io)
- [Human-In-the-Loop MCP Server](https://github.com/GongRzhe/Human-In-the-Loop-MCP-Server)
- [Amazon Bedrock Agents Human-in-the-Loop](https://aws.amazon.com/blogs/machine-learning/implement-human-in-the-loop-confirmation-with-amazon-bedrock-agents/)

---

*Pattern documented: 2025-01-16*
