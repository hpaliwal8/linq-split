package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hpaliwal8/linq-split/internal/db"
	"github.com/hpaliwal8/linq-split/internal/linq"
	"github.com/hpaliwal8/linq-split/internal/parser"
	"github.com/hpaliwal8/linq-split/internal/settle"
)

// Config holds all dependencies for the webhook handler.
type Config struct {
	LinqClient    *linq.Client
	WebhookSecret string
	Parser        *parser.ClaudeParser
	Store         *db.Store
}

// HandleWebhook processes inbound Linq webhook events.
func (c *Config) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// ── Verify webhook signature ─────────────────────────────────
	timestamp := r.Header.Get("X-Linq-Timestamp")
	signature := r.Header.Get("X-Linq-Signature")
	if !linq.VerifySignature(body, timestamp, signature, c.WebhookSecret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// ── Parse the event ──────────────────────────────────────────
	var event linq.InboundEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	// Respond immediately — process async
	w.WriteHeader(http.StatusOK)

	// Only handle text messages
	if event.EventType != "message.received" {
		return
	}
	if event.Data.Message == nil || len(event.Data.Message.Parts) == 0 {
		return
	}

	text := event.Data.Message.Parts[0].Value
	chatID := event.Data.ChatID
	senderHandle := ""
	senderName := ""
	if event.Data.Sender != nil {
		senderHandle = event.Data.Sender.Handle
		senderName = event.Data.Sender.Name
	}

	// Process in a goroutine so the webhook returns fast
	go c.processMessage(chatID, text, senderHandle, senderName, event.Data.Message.ID)
}

func (c *Config) processMessage(chatID, text, senderHandle, senderName, messageID string) {
	// Show typing indicator while we work
	_ = c.LinqClient.StartTyping(chatID)
	defer c.LinqClient.StopTyping(chatID)

	// ── Ensure group + sender exist ──────────────────────────────
	groupID, err := c.Store.EnsureGroup(chatID)
	if err != nil {
		log.Printf("error ensuring group: %v", err)
		return
	}

	_, err = c.Store.EnsureMember(groupID, senderHandle, senderName)
	if err != nil {
		log.Printf("error ensuring member: %v", err)
		return
	}

	// Get group members for context
	members, err := c.Store.GetGroupMembers(groupID)
	if err != nil {
		log.Printf("error getting members: %v", err)
		return
	}

	// ── Parse the message with Claude ────────────────────────────
	parsed, err := c.Parser.Parse(text, senderHandle, members)
	if err != nil {
		log.Printf("error parsing message: %v", err)
		return
	}

	// ── Route by intent ──────────────────────────────────────────
	var reply string

	switch parsed.Intent {
	case parser.IntentAddExpense:
		reply, err = c.handleAddExpense(groupID, parsed)
	case parser.IntentCustomSplit:
		reply, err = c.handleCustomSplit(groupID, parsed)
	case parser.IntentCheckBalance:
		reply, err = c.handleCheckBalance(groupID)
	case parser.IntentSettle:
		reply, err = c.handleSettle(groupID, parsed)
	case parser.IntentQuery:
		reply, err = c.handleQuery(groupID, parsed)
	case parser.IntentIgnore:
		return // not an expense-related message
	}

	if err != nil {
		log.Printf("error handling %s: %v", parsed.Intent, err)
		reply = "Something went wrong processing that. Try again?"
	}

	if reply != "" {
		if err := c.LinqClient.SendText(chatID, reply); err != nil {
			log.Printf("error sending reply: %v", err)
		}
	}

	// React to the original message to confirm it was logged
	if parsed.Intent == parser.IntentAddExpense || parsed.Intent == parser.IntentCustomSplit {
		_ = c.LinqClient.React(chatID, messageID, "like")
	}
	if parsed.Intent == parser.IntentSettle {
		_ = c.LinqClient.React(chatID, messageID, "love")
	}
}

// ── Intent handlers ──────────────────────────────────────────────────

func (c *Config) handleAddExpense(groupID int64, p *parser.ParsedMessage) (string, error) {
	payerID, err := c.Store.GetMemberByHandle(groupID, p.Payer)
	if err != nil {
		return "", fmt.Errorf("unknown payer %s: %w", p.Payer, err)
	}

	allMembers, err := c.Store.GetAllMembers(groupID)
	if err != nil {
		return "", err
	}

	// Even split across all members
	splitAmt := p.Amount / float64(len(allMembers))
	splits := make(map[int64]float64)
	for _, m := range allMembers {
		splits[m.ID] = splitAmt
	}

	_, err = c.Store.AddExpense(groupID, payerID, p.Amount, p.Description, p.Category, splits)
	if err != nil {
		return "", err
	}

	payerInfo, _ := c.Store.GetMemberInfo(payerID)
	name := displayName(payerInfo)
	return fmt.Sprintf(
		"Got it! %s paid $%.2f for %s — split evenly (%d ways, $%.2f each).",
		name, p.Amount, p.Description, len(allMembers), splitAmt,
	), nil
}

func (c *Config) handleCustomSplit(groupID int64, p *parser.ParsedMessage) (string, error) {
	payerID, err := c.Store.GetMemberByHandle(groupID, p.Payer)
	if err != nil {
		return "", fmt.Errorf("unknown payer %s: %w", p.Payer, err)
	}

	allMembers, err := c.Store.GetAllMembers(groupID)
	if err != nil {
		return "", err
	}

	splits := make(map[int64]float64)

	if len(p.CustomSplit) > 0 {
		// Explicit amounts per person
		for handle, amt := range p.CustomSplit {
			memberID, err := c.Store.GetMemberByHandle(groupID, handle)
			if err != nil {
				continue
			}
			splits[memberID] = amt
		}
	} else {
		// Even split excluding certain members
		excluded := make(map[string]bool)
		for _, h := range p.Excluded {
			excluded[h] = true
		}

		var includedMembers []*db.MemberInfo
		for _, m := range allMembers {
			if !excluded[m.Handle] {
				includedMembers = append(includedMembers, m)
			}
		}

		if len(includedMembers) == 0 {
			return "Can't split an expense with no one included!", nil
		}

		splitAmt := p.Amount / float64(len(includedMembers))
		for _, m := range includedMembers {
			splits[m.ID] = splitAmt
		}
	}

	_, err = c.Store.AddExpense(groupID, payerID, p.Amount, p.Description, p.Category, splits)
	if err != nil {
		return "", err
	}

	payerInfo, _ := c.Store.GetMemberInfo(payerID)
	return fmt.Sprintf(
		"Got it! %s paid $%.2f for %s — custom split across %d people.",
		displayName(payerInfo), p.Amount, p.Description, len(splits),
	), nil
}

func (c *Config) handleCheckBalance(groupID int64) (string, error) {
	netBalances, err := c.Store.GetNetBalances(groupID)
	if err != nil {
		return "", err
	}

	debts := settle.Simplify(netBalances)

	nameFunc := func(id int64) string {
		info, err := c.Store.GetMemberInfo(id)
		if err != nil {
			return "unknown"
		}
		return displayName(info)
	}

	return settle.FormatDebts(debts, nameFunc), nil
}

func (c *Config) handleSettle(groupID int64, p *parser.ParsedMessage) (string, error) {
	fromID, err := c.Store.GetMemberByHandle(groupID, p.SettleFrom)
	if err != nil {
		return "", fmt.Errorf("unknown member %s: %w", p.SettleFrom, err)
	}

	toID, err := c.Store.GetMemberByHandle(groupID, p.SettleTo)
	if err != nil {
		return "", fmt.Errorf("unknown member %s: %w", p.SettleTo, err)
	}

	if err := c.Store.AddSettlement(groupID, fromID, toID, p.Amount); err != nil {
		return "", err
	}

	fromInfo, _ := c.Store.GetMemberInfo(fromID)
	toInfo, _ := c.Store.GetMemberInfo(toID)
	return fmt.Sprintf(
		"Settled! %s paid %s $%.2f. Balances updated.",
		displayName(fromInfo), displayName(toInfo), p.Amount,
	), nil
}

func (c *Config) handleQuery(groupID int64, p *parser.ParsedMessage) (string, error) {
	since, label := timeRangeFromQuery(p.QueryText)

	categories, total, err := c.Store.GetSpendingSince(groupID, since)
	if err != nil {
		return "", err
	}

	if total == 0 {
		return fmt.Sprintf("No expenses recorded %s.", label), nil
	}

	// Sort categories by amount descending
	type catAmt struct {
		cat string
		amt float64
	}
	var sorted []catAmt
	for cat, amt := range categories {
		sorted = append(sorted, catAmt{cat, amt})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].amt > sorted[j].amt
	})

	// Check if the query mentions a specific category
	focusCategory := categoryFromQuery(p.QueryText)

	var result string
	if focusCategory != "" {
		amt := categories[focusCategory]
		if amt == 0 {
			return fmt.Sprintf("Nothing spent on %s %s.", focusCategory, label), nil
		}
		result = fmt.Sprintf("Spent $%.2f on %s %s.", amt, focusCategory, label)
	} else {
		result = fmt.Sprintf("Spending %s — $%.2f total:\n\n", label, total)
		for _, ca := range sorted {
			pct := (ca.amt / total) * 100
			result += fmt.Sprintf("  %-16s $%.2f (%.0f%%)\n", ca.cat, ca.amt, pct)
		}
	}

	return result, nil
}

// timeRangeFromQuery detects a time window from natural language.
// Returns the start time and a human-readable label.
func timeRangeFromQuery(q string) (time.Time, string) {
	q = strings.ToLower(q)
	now := time.Now()

	if strings.Contains(q, "week") {
		return now.AddDate(0, 0, -7), "in the last 7 days"
	}
	if strings.Contains(q, "month") {
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), "this month"
	}
	if strings.Contains(q, "year") {
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location()), "this year"
	}
	return time.Time{}, "all time"
}

// categoryFromQuery checks if the query names a specific spending category.
func categoryFromQuery(q string) string {
	q = strings.ToLower(q)
	known := []string{"food", "groceries", "drinks", "transport", "utilities", "rent", "entertainment", "travel"}
	for _, cat := range known {
		if strings.Contains(q, cat) {
			return cat
		}
	}
	return ""
}

// displayName returns the best available display name for a member.
func displayName(m *db.MemberInfo) string {
	if m == nil {
		return "someone"
	}
	if m.Name != "" {
		return m.Name
	}
	return m.Handle
}
