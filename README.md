# Split — Expense Splitter That Lives in Group Chat

An iMessage agent that tracks shared expenses, calculates balances, and settles debts — right inside your group chat. No app downloads, no sign-ups.

Built with Go, powered by the [Linq Blue API](https://linqapp.com) and [Claude](https://anthropic.com).

## How It Works

Add the Split agent to an iMessage group chat. Then just text naturally:

| Message | What happens |
|---------|-------------|
| `$47.50 groceries` | Splits evenly across the group |
| `$60 dinner, exclude @Jake` | Splits among everyone except Jake |
| `$45 pizza, @Hitansh owes $20, @Mike owes $25` | Custom split amounts |
| `who owes what?` | Shows simplified outstanding balances |
| `@Jake paid @Hitansh $30` | Records a settlement payment |

The agent uses Claude to parse natural language, so you don't need exact formats — just text like you normally would.

## Architecture

```
iMessage Group Chat
       │
       ▼
  Linq Webhook ──► POST /webhook
       │
       ▼
  Claude API ──► Parse intent + extract amounts
       │
       ▼
  SQLite ──► Update balances
       │
       ▼
  Linq Send API ──► Reply to group + tapback reaction
```

## Project Structure

```
linq-split/
├── cmd/linq-split/main.go           # Entry point, HTTP server
├── internal/
│   ├── linq/client.go          # Linq API client (send, react, typing)
│   ├── parser/claude.go        # Claude-powered NLP expense parser
│   ├── db/store.go             # SQLite storage layer
│   ├── settle/simplify.go      # Debt simplification algorithm
│   ├── settle/simplify_test.go # Tests for settlement logic
│   └── handlers/webhook.go     # Webhook handler + intent routing
├── .env.example
├── Makefile
└── go.mod
```

## Quick Start

### Prerequisites

- Go 1.22+
- A [Linq](https://linqapp.com) account with API token and provisioned phone number
- An [Anthropic](https://console.anthropic.com) API key
- [ngrok](https://ngrok.com) (for local development)

### Setup

```bash
# Clone and build
git clone https://github.com/hpaliwal8/linq-split.git
cd linq-split
cp .env.example .env
# Fill in your API keys in .env

# Source env vars
export $(cat .env | xargs)

# Build and run
make run
```

### Connect to Linq

```bash
# In a separate terminal, expose your server
make tunnel

# Register your webhook with Linq
curl -X POST https://api.linqapp.com/api/partner/v3/webhook-subscriptions \
  -H "Authorization: Bearer $LINQ_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "target_url": "https://YOUR-NGROK-URL/webhook",
    "subscribed_events": [
      "message.received",
      "reaction.added"
    ]
  }'
```

Save the `signing_secret` from the response into your `.env` as `LINQ_WEBHOOK_SECRET`.

### Create a Group Chat

Add your Linq phone number to an iMessage group chat with friends. Start logging expenses!

## Debt Simplification

The settlement algorithm minimizes the number of transactions needed. Instead of tracking every pairwise debt, it:

1. Calculates each person's **net balance** (total paid - total owed)
2. Greedily matches the **largest creditor** with the **largest debtor**
3. Produces the minimum set of payments to settle everyone

Example: If A owes B $10 and B owes C $10, it simplifies to **A owes C $10** (1 transaction instead of 2).

## Tech Choices

| Choice | Why |
|--------|-----|
| **Go** | Single binary, no runtime deps, natural concurrency for webhook processing |
| **net/http** | 3 routes don't need a framework |
| **SQLite** | Zero-config, perfect for single-server deployment |
| **Claude API** | Best-in-class natural language understanding for expense parsing |
| **Linq** | Native iMessage group chat with reactions + typing indicators |

## Development

```bash
make test     # Run tests
make dev      # Run with auto-reload (requires air)
make tunnel   # Expose local server via ngrok
```

## License

MIT
