package linq

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const baseURL = "https://api.linqapp.com/api/partner/v3"

// Client wraps the Linq Partner API v3.
type Client struct {
	token      string
	fromNumber string
	http       *http.Client
}

func NewClient(token, fromNumber string) *Client {
	return &Client{
		token:      token,
		fromNumber: fromNumber,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

// ── Webhook payload types ────────────────────────────────────────────

// InboundEvent is the top-level webhook payload from Linq.
type InboundEvent struct {
	EventType string       `json:"event_type"` // "message.received", "reaction.added", etc.
	Data      EventData    `json:"data"`
}

type EventData struct {
	ChatID  string   `json:"chat_id"`
	Message *Message `json:"message,omitempty"`
	Sender  *Sender  `json:"sender,omitempty"`
}

type Message struct {
	ID    string        `json:"id"`
	Parts []MessagePart `json:"parts"`
}

type MessagePart struct {
	Type  string `json:"type"`  // "text", "attachment", etc.
	Value string `json:"value"` // the actual text content
}

type Sender struct {
	Handle string `json:"handle"` // phone number in E.164
	Name   string `json:"name"`   // contact name if available
}

// ── Sending messages ─────────────────────────────────────────────────

type sendRequest struct {
	ChatID  string `json:"chat_id"`
	From    string `json:"from"`
	Message struct {
		Parts []MessagePart `json:"parts"`
	} `json:"message"`
}

// SendText sends a text message to a chat.
func (c *Client) SendText(chatID, text string) error {
	req := sendRequest{
		ChatID: chatID,
		From:   c.fromNumber,
	}
	req.Message.Parts = []MessagePart{{Type: "text", Value: text}}
	return c.post("/messages", req)
}

// ── Typing indicators ────────────────────────────────────────────────

type typingRequest struct {
	ChatID string `json:"chat_id"`
	From   string `json:"from"`
	Action string `json:"action"` // "start" or "stop"
}

// StartTyping shows the typing indicator in the chat.
func (c *Client) StartTyping(chatID string) error {
	return c.post("/typing-indicators", typingRequest{
		ChatID: chatID,
		From:   c.fromNumber,
		Action: "start",
	})
}

// StopTyping hides the typing indicator.
func (c *Client) StopTyping(chatID string) error {
	return c.post("/typing-indicators", typingRequest{
		ChatID: chatID,
		From:   c.fromNumber,
		Action: "stop",
	})
}

// ── Reactions (tapbacks) ─────────────────────────────────────────────

type reactionRequest struct {
	ChatID    string `json:"chat_id"`
	From      string `json:"from"`
	MessageID string `json:"message_id"`
	Reaction  string `json:"reaction"` // "love", "like", "dislike", "laugh", "emphasize", "question"
}

// React adds a tapback reaction to a message.
func (c *Client) React(chatID, messageID, reaction string) error {
	return c.post("/reactions", reactionRequest{
		ChatID:    chatID,
		From:      c.fromNumber,
		MessageID: messageID,
		Reaction:  reaction,
	})
}

// ── Webhook signature verification ──────────────────────────────────

// VerifySignature checks the HMAC-SHA256 signature of an inbound webhook.
func VerifySignature(payload []byte, timestamp, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// ── HTTP helper ──────────────────────────────────────────────────────

func (c *Client) post(path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("linq API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("linq API %s returned %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}
