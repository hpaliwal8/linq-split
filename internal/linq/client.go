package linq

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	EventType string    `json:"event_type"` // "message.received", "reaction.added", etc.
	Data      EventData `json:"data"`
}

// EventData is the actual Linq v3 message payload shape.
// The message fields (id, parts, sender_handle) sit directly under data,
// and the chat info is nested under data.chat.
type EventData struct {
	Chat         *Chat         `json:"chat"`
	ID           string        `json:"id"`            // message ID
	Parts        []MessagePart `json:"parts"`
	SenderHandle *Sender       `json:"sender_handle"`
	Direction    string        `json:"direction"` // "inbound" or "outbound"
}

type Chat struct {
	ID      string `json:"id"`
	IsGroup bool   `json:"is_group"`
}

type MessagePart struct {
	Type  string `json:"type"`  // "text", "attachment", etc.
	Value string `json:"value"` // the actual text content
}

// MessageEffect is an optional iMessage effect applied at the message level.
type MessageEffect struct {
	Type string `json:"type"` // "screen" or "bubble"
	Name string `json:"name"` // e.g. "confetti", "fireworks", "balloons"
}

type Sender struct {
	Handle string `json:"handle"` // phone number in E.164
	Name   string `json:"name"`   // contact name if available
}

// ── Contact cards ────────────────────────────────────────────────────

type ContactCard struct {
	PhoneNumber string `json:"phone_number"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	ImageURL    string `json:"image_url"`
	IsActive    bool   `json:"is_active"`
}

// GetContactCard fetches the contact card for a phone number.
// Returns nil if no card is found.
func (c *Client) GetContactCard(phoneNumber string) (*ContactCard, error) {
	req, err := http.NewRequest("GET", baseURL+"/contact_card", nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("phone_number", phoneNumber)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, nil // not found — not fatal
	}

	var result struct {
		ContactCards []ContactCard `json:"contact_cards"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.ContactCards) == 0 {
		return nil, nil
	}
	return &result.ContactCards[0], nil
}

// ── Sending messages ─────────────────────────────────────────────────

type sendRequest struct {
	ChatID  string `json:"chat_id"`
	From    string `json:"from"`
	Message struct {
		Parts  []MessagePart  `json:"parts"`
		Effect *MessageEffect `json:"effect,omitempty"`
	} `json:"message"`
}

// SendText sends a text message to a chat, with an optional iMessage screen effect.
func (c *Client) SendText(chatID, text string, effect ...string) error {
	req := sendRequest{ChatID: chatID, From: c.fromNumber}
	req.Message.Parts = []MessagePart{{Type: "text", Value: text}}
	if len(effect) > 0 && effect[0] != "" {
		req.Message.Effect = &MessageEffect{Type: "screen", Name: effect[0]}
	}
	return c.post("/chats/"+chatID+"/messages", req)
}

// ── Typing indicators ────────────────────────────────────────────────
// Note: typing indicators are not supported in group chats (Linq returns 403).

// StartTyping shows the typing indicator in the chat.
func (c *Client) StartTyping(chatID string) error {
	return c.post("/chats/"+chatID+"/typing", nil)
}

// StopTyping hides the typing indicator.
func (c *Client) StopTyping(chatID string) error {
	return c.delete("/chats/" + chatID + "/typing")
}

// ── Reactions (tapbacks) ─────────────────────────────────────────────

// React adds a tapback reaction to a message.
// reaction must be one of: love, like, dislike, laugh, emphasize, question
func (c *Client) React(messageID, reaction string) error {
	body := struct {
		Operation string `json:"operation"`
		Type      string `json:"type"`
	}{
		Operation: "add",
		Type:      reaction,
	}
	return c.post("/messages/"+messageID+"/reactions", body)
}

// ── Webhook signature verification ──────────────────────────────────

// VerifySignature checks the HMAC-SHA256 signature of an inbound webhook.
// Signed string is "{timestamp}.{rawBody}"; secret is used as-is.
func VerifySignature(payload []byte, timestamp, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(payload)))
	expected := hex.EncodeToString(mac.Sum(nil))
	log.Printf("DEBUG sig: expected=%s got=%s match=%v", expected, signature, expected == signature)
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

func (c *Client) delete(path string) error {
	req, err := http.NewRequest("DELETE", baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

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
