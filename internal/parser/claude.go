package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Intent represents what the user is trying to do.
type Intent string

const (
	IntentAddExpense   Intent = "add_expense"
	IntentCustomSplit  Intent = "custom_split"
	IntentCheckBalance Intent = "check_balance"
	IntentSettle       Intent = "settle"
	IntentQuery        Intent = "query"
	IntentRegister     Intent = "register"
	IntentVoidExpense  Intent = "void_expense"
	IntentEditExpense  Intent = "edit_expense"
	IntentIgnore       Intent = "ignore"
)

// ParsedMessage is the structured output from the LLM.
type ParsedMessage struct {
	Intent      Intent             `json:"intent"`
	Amount      float64            `json:"amount,omitempty"`
	Description string             `json:"description,omitempty"`
	Category    string             `json:"category,omitempty"`
	Payer       string             `json:"payer,omitempty"`        // handle/phone of who paid
	Excluded    []string           `json:"excluded,omitempty"`     // handles to exclude from split
	CustomSplit map[string]float64 `json:"custom_split,omitempty"` // handle -> specific amount owed
	SettleFrom    string             `json:"settle_from,omitempty"`
	SettleTo      string             `json:"settle_to,omitempty"`
	QueryText     string             `json:"query_text,omitempty"`
	RegisterName   string             `json:"register_name,omitempty"`
	ExpenseRef     string             `json:"expense_ref,omitempty"`
	NewAmount      float64            `json:"new_amount,omitempty"`
	NewDescription string             `json:"new_description,omitempty"`
	Confidence     float64            `json:"confidence"`
}

// ClaudeParser calls the Anthropic API to parse expense messages.
type ClaudeParser struct {
	apiKey string
	http   *http.Client
}

func NewClaudeParser(apiKey string) *ClaudeParser {
	return &ClaudeParser{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

const systemPrompt = `You are an expense-parsing agent in an iMessage group chat called "Split".
Your job is to classify incoming messages and extract structured data.

Members are identified by phone number handles (E.164 format like +15551234567) or by @name mentions.

Classify each message into exactly one intent:

1. "add_expense" — someone logs an expense to split evenly.
   Examples: "$47.50 groceries", "paid 120 for electric bill", "I got dinner $85"
   Extract: amount, description, category (infer from description), payer (the sender).

2. "custom_split" — expense with uneven or partial split.
   Examples: "$60 dinner, exclude @Jake", "$45 pizza, @Hitansh owes $20, @Mike owes $25"
   Also handles parts/ratio splits — compute the dollar amounts yourself before returning.
   Example: "$40 groceries, @Alice 3 parts @Bob 1 part" → total 4 parts, Alice=$30, Bob=$10 → custom_split: {"@Alice": 30, "@Bob": 10}
   Extract: amount, description, category, payer, excluded list OR custom_split map (always in dollars).

3. "check_balance" — user wants to see who owes what.
   Examples: "who owes what?", "balances", "what's the tally?"

4. "settle" — someone records a payment between two people.
   Examples: "@Jake paid @Hitansh $30", "I sent Mike $50"
   Extract: amount, settle_from, settle_to.

5. "query" — question about spending history.
   Examples: "how much have we spent this month?", "what did we spend on food?"
   Extract: query_text.

6. "register" — someone is telling the bot their name.
   Examples: "I'm Hitansh", "call me Jake", "my name is Sarah"
   Extract: register_name (just the name, no extra words).

7. "void_expense" — cancel or delete a previously logged expense.
   Examples: "remove the last expense", "delete the pizza charge", "undo that $47 groceries"
   Extract: expense_ref ("last" if most recent, otherwise the description or amount mentioned).

8. "edit_expense" — change the amount or description of a previous expense.
   Examples: "change last expense to $35", "the groceries were actually $52", "update pizza to $30"
   Extract: expense_ref, new_amount (if changing amount), new_description (if changing description).

9. "ignore" — not related to expenses at all.
   Examples: "lol", "anyone want to grab coffee?", "good morning"

Respond with ONLY valid JSON. No markdown, no explanation. Use this exact schema:
{
  "intent": "add_expense|custom_split|check_balance|settle|query|register|void_expense|edit_expense|ignore",
  "amount": 0.00,
  "description": "",
  "category": "",
  "payer": "",
  "excluded": [],
  "custom_split": {},
  "settle_from": "",
  "settle_to": "",
  "query_text": "",
  "register_name": "",
  "expense_ref": "",
  "new_amount": 0.00,
  "new_description": "",
  "confidence": 0.95
}

Set confidence between 0 and 1. If below 0.7, set intent to "ignore" — we'd rather miss an expense than log a wrong one.

You MUST respond with raw JSON only. Do not use markdown code fences, do not include ` + "```" + `json, do not add any explanation or commentary. Your entire response must be directly parseable by JSON.parse().`

// Parse sends a message to Claude and returns structured expense data.
func (p *ClaudeParser) Parse(text, senderHandle string, groupMembers []string) (*ParsedMessage, error) {
	userPrompt := fmt.Sprintf(
		"Sender: %s\nGroup members: %v\nMessage: %s",
		senderHandle, groupMembers, text,
	)

	reqBody := map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 512,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal claude request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create claude request: %w", err)
	}
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude API call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read claude response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("claude API returned %d: %s", resp.StatusCode, string(body))
	}

	// Extract text from Claude's response format
	var claudeResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &claudeResp); err != nil {
		return nil, fmt.Errorf("unmarshal claude response: %w", err)
	}

	if len(claudeResp.Content) == 0 {
		return nil, fmt.Errorf("empty claude response")
	}

	// Parse the JSON from Claude's text output
	var parsed ParsedMessage
	responseText := claudeResp.Content[0].Text
	if err := json.Unmarshal([]byte(responseText), &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal parsed message (raw: %s): %w", responseText, err)
	}

	// Enforce confidence threshold — treat low-confidence parses as ignored
	if parsed.Confidence < 0.7 {
		parsed.Intent = IntentIgnore
	}

	// Default payer to sender if not specified
	if parsed.Payer == "" && (parsed.Intent == IntentAddExpense || parsed.Intent == IntentCustomSplit) {
		parsed.Payer = senderHandle
	}

	return &parsed, nil
}
