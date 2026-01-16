# Gmail MCP Server

An MCP server that lets AI agents search Gmail threads, understand your email writing style, and create draft emails.

## 1. Get Google Authentication

### Step 1: Create a Google Cloud Project
1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Click "Select a project" dropdown at the top
3. Click "New Project"
4. Enter a project name (e.g., "Gmail MCP Server")
5. Click "Create"

### Step 2: Enable Gmail API
1. In your new project, go to "APIs & Services" → "Library"
2. Search for "Gmail API"
3. Click on "Gmail API" and click "Enable"

### Step 3: Create OAuth2 Credentials
1. Go to "APIs & Services" → "Credentials"
2. Click "Create Credentials" → "OAuth Client ID"
3. If prompted, configure the OAuth consent screen:
   - Choose "External" user type
   - Fill in required fields (App name, User support email, Developer email)
   - Add your email to "Test users" section
   - Save and continue through all steps
4. Back in Credentials, click "Create Credentials" → "OAuth Client ID"
5. Choose "Desktop application" as the application type
6. Enter a name (e.g., "Gmail MCP Client")
7. Click "Create"
8. **Important**: Copy the **Client ID** and **Client Secret** from the confirmation dialog (you'll need these for configuration)

### Step 4: Grant OAuth Scopes
When you first run the server, it will open your browser for authorization. The server requests **only these minimal permissions**:

#### What We Request:
- ✅ **Gmail Readonly Access** (`gmail.readonly`)
  - Search and read your email messages
  - Download email attachments  
  - View email metadata (subjects, senders, dates)

- ✅ **Gmail Compose Access** (`gmail.compose`)
  - Create email drafts
  - Update existing drafts
  - Delete drafts
  - **Send emails** (permission granted but not used by this server)

#### What This Server Actually Implements:
- ✅ **Search and read emails** - Full search capabilities
- ✅ **Extract attachment text** - Safe PDF/DOCX/TXT text extraction
- ✅ **Create/update drafts** - Smart draft management with thread awareness
- ✅ **Send emails** - With secure OOB approval (see below)
- ❌ **Delete emails** - Server doesn't implement deletion
- ❌ **Modify labels** - Server doesn't implement label management

## 2. Add to MCP Clients

Build the server first: `go build .`

You'll want to add this to your agent's configuration file:

```json
{
  "mcpServers": {
    "gmail": {
      "command": "C:/path/to/your/auto-gmail.exe",
      "env": {
        "GMAIL_CLIENT_ID": "your_client_id_here.apps.googleusercontent.com",
        "GMAIL_CLIENT_SECRET": "your_client_secret_here",
        "OPENAI_API_KEY": "your_openai_api_key_here"
      }
    }
  }
}
```

### ⚡ **Persistent HTTP Mode (Recommended to Avoid OAuth Popups)**

**Problem**: In stdio mode, Cursor starts a fresh server process each time (and for each tab), causing OAuth popup spam.

**Solution**: Run the server as a persistent HTTP daemon that authenticates once and stays running.

#### Quick Start:
```bash
# Build the server
go build -o gmail-mcp-server

# Start persistent server (OAuth only once!)
./gmail-mcp-server --http

# Or with custom port
./gmail-mcp-server --http 3000
```

#### What Happens:
1. ✅ **OAuth popup appears ONCE** when server starts
2. ✅ **Server runs persistently** on http://localhost:8080  
3. ✅ **No more OAuth popups** - server stays authenticated
4. ✅ **Multiple Cursor tabs/windows** can connect to same server

#### Cursor Configuration (Still Use stdio for now):
```json
{
  "mcpServers": {
    "gmail": {
      "command": "C:/path/to/your/gmail-mcp-server",
      "env": {
        "GMAIL_CLIENT_ID": "your_client_id_here.apps.googleusercontent.com", 
        "GMAIL_CLIENT_SECRET": "your_client_secret_here",
        "OPENAI_API_KEY": "your_openai_api_key_here"
      }
    }
  }
}
```

The difference: **You start the server manually once** instead of letting Cursor start it fresh each time.

#### Server Status:
- Visit http://localhost:8080 to see server status
- Health check: http://localhost:8080/health
- View available tools and configuration examples

### Add to Cursor
- Press `Ctrl+Shift+P` (Windows/Linux) or `Cmd+Shift+P` (Mac)
- Click the MCP-tab
- Click '+ Add new global MCP server'
- Edit config file

### Add to Claude Desktop
- Go to File > Settings > Developer > Edit Config
- Edit Config file

### Manual Configuration Alternative:
You can edit these config files directly if you know where to find them:

- **Cursor:** `C:\Users\[User]\.cursor\mcp.json`
- **Claude Desktop:** `%APPDATA%\Claude\claude_desktop_config.json` (Windows)

## 3. MCP Tools and Resources

**Tools:**
- `search_threads` - Search Gmail with queries like "from:email@example.com" or "subject:meeting" (includes draft info)
- `create_draft` - Create email drafts or update existing drafts (AI will request style guide first)
- `send_draft` - Submit a draft for user approval and sending (see **Secure Email Sending** below)
- `fetch_email_bodies` - Get full email content for specific threads
- `extract_attachment_by_filename` - Safely extract text from PDF, DOCX, and TXT attachments using filename
- `get_personal_email_style_guide` - Get your email writing style guide (temporary tool until agents support MCP resources better)

**Resources:**
- `file://personal-email-style-guide` - Your personal email writing style (auto-generated or manual)

**Prompts:**
- `/generate-email-tone` - Analyze your sent emails to create personalized writing style
- `/server-status` - Show file locations and server status

## 4. Personal Email Style Guide

The server will create a style-guide file based on the last 25 emails you've sent, so that newly drafted emails will hopefully sound like you. Honestly, so far LLM-written emails still don't sound very authentic.

**Manual Generation:**
- Run `/generate-email-tone` prompt in your MCP client anytime to regenerate
- The file is saved to your app data directory (see **File Storage Locations** above)

**AI Integration:**
- AI always calls `get_personal_email_style_guide` tool before writing emails
- Ensures consistent personal style across all communications
- Resource also available at `file://personal-email-style-guide`

## 5. Secure Email Sending (Agent Cut-Out Pattern)

This server implements a security pattern called **Agent Cut-Out** that prevents AI agents from misrepresenting email content to users.

### The Problem
When an AI agent sends emails on your behalf, it could potentially:
- Show you a "preview" that differs from what actually gets sent
- Manipulate you into approving emails through social engineering
- Send emails without genuine human approval

### The Solution: Out-of-Band (OOB) Approval

When the server starts, you'll see:

```
═══════════════════════════════════════════════════════════════
📤 OOB APPROVAL DASHBOARD (Agent Cut-Out Pattern)
═══════════════════════════════════════════════════════════════
   http://localhost:8787/outbox/x7k2m9p4q1w8e5r2t6y3u0i9

   Open this URL in your browser to approve outgoing emails.
   The agent CANNOT see or influence this approval process.
   Keep this tab open while working with your AI assistant.
═══════════════════════════════════════════════════════════════
```

### How It Works

1. **Agent creates a draft** using `create_draft`
2. **Agent calls `send_draft`** - this call BLOCKS
3. **You see the ACTUAL email** on the web dashboard (not what the agent claims)
4. **You click Approve or Reject** in your browser
5. **Server sends the email** (if approved) and notifies the agent

### Security Properties

| Property | Description |
|----------|-------------|
| **Agent-blind** | Agent cannot see or influence the approval UI |
| **Server-executed** | Server sends the email, not the agent |
| **One-at-a-time** | Only one email can be pending approval |
| **Time-limited** | Pending emails expire after 5 minutes |
| **Tamper-proof** | You see exactly what the server will send |

### Usage

1. Start the MCP server
2. Open the dashboard URL in your browser (keep it open)
3. Work with your AI agent normally
4. When the agent wants to send an email, review and approve/reject in the dashboard

See `docs/agent-cut-out-pattern.md` for the full security pattern documentation.

## 7. Alternative way to Setup Environment Variables

If you want to run this MCP server outside of an agent, you can create a .env file based on the .env.example file and supply the environment variables that way, or export them into your environment prior to running:

```bash
export GMAIL_CLIENT_ID=your_client_id_here.apps.googleusercontent.com
export GMAIL_CLIENT_SECRET=your_client_secret_here
export OPENAI_API_KEY=your_openai_api_key_here
```

## 8. File Storage Locations

The server stores authentication and configuration files in standard application directories:

### File Locations:
- **Windows**: `C:\Users\[YourUsername]\AppData\Roaming\auto-gmail\`
- **Mac**: `~/.auto-gmail/`  
- **Linux**: `~/.auto-gmail/`

### Important Files:
- **`token.json`** - OAuth authentication token (auto-generated)
- **`personal-email-style-guide.md`** - Your email writing style guide (auto-generated or manual)

### Quick Commands:
- Use `/server-status` in your MCP client to see exact file paths
- Delete `token.json` to force re-authentication with updated permissions

## 7. TODOs

- [x] **Improve OAuth login flow** - ✅ **SOLVED!** Use persistent HTTP mode (`./gmail-mcp-server --http`) to avoid OAuth popups. Server authenticates once and stays running.
- [ ] **Full HTTP MCP Transport** - Waiting for mark3labs/mcp-go to expose complete HTTP transport APIs
- [ ] **Better Email Authenticity** - LLM-written emails still don't sound perfectly authentic despite style guides
