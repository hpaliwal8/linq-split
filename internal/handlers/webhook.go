package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
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
	log.Printf("webhook received from %s", r.RemoteAddr)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// ── Verify webhook signature ─────────────────────────────────
	timestamp := r.Header.Get("X-Webhook-Timestamp")
	signature := r.Header.Get("X-Webhook-Signature")
	if !linq.VerifySignature(body, timestamp, signature, c.WebhookSecret) {
		log.Printf("signature mismatch — timestamp=%s len(body)=%d secret_len=%d secret_last4=%q",
			timestamp, len(body), len(c.WebhookSecret), c.WebhookSecret[max(0, len(c.WebhookSecret)-4):])
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

	// Only handle inbound text messages
	if event.EventType != "message.received" {
		return
	}
	if event.Data.Direction == "outbound" {
		return // ignore our own messages
	}
	if len(event.Data.Parts) == 0 || event.Data.Chat == nil {
		return
	}

	text := event.Data.Parts[0].Value
	chatID := event.Data.Chat.ID
	messageID := event.Data.ID
	senderHandle := ""
	senderName := ""
	if event.Data.SenderHandle != nil {
		senderHandle = event.Data.SenderHandle.Handle
		senderName = event.Data.SenderHandle.Name
	}

	// Ignore messages with no identifiable sender
	if senderHandle == "" {
		log.Printf("skipping message with no sender handle")
		return
	}

	// Process in a goroutine so the webhook returns fast
	go c.processMessage(chatID, text, senderHandle, senderName, messageID)
}

func (c *Config) processMessage(chatID, text, senderHandle, senderName, messageID string) {
	// Show typing indicator while we work
	_ = c.LinqClient.StartTyping(chatID)
	defer c.LinqClient.StopTyping(chatID)

	// ── Ensure group + sender exist ──────────────────────────────
	groupID, isNew, err := c.Store.EnsureGroup(chatID)
	if err != nil {
		log.Printf("error ensuring group: %v", err)
		return
	}

	if isNew {
		welcome := "👋 Hi! I'm your group expense splitter. I'll track who paid what and keep balances up to date.\n\nTo get started, everyone please introduce yourself: \"I'm Jake\""
		_ = c.LinqClient.SendText(chatID, welcome)
	}

	memberID, err := c.Store.EnsureMember(groupID, senderHandle, senderName)
	if err != nil {
		log.Printf("error ensuring member: %v", err)
		return
	}

	// Resolve display name from contact card if we don't have one yet
	if senderName == "" && senderHandle != "" {
		if card, err := c.LinqClient.GetContactCard(senderHandle); err == nil && card != nil {
			name := strings.TrimSpace(card.FirstName + " " + card.LastName)
			if name != "" {
				_ = c.Store.UpdateMemberName(memberID, name)
				senderName = name
			}
		}
	}

	// Build name→handle context for the parser
	allMembers, err := c.Store.GetAllMembers(groupID)
	if err != nil {
		log.Printf("error getting members: %v", err)
		return
	}
	memberContext := make([]string, 0, len(allMembers))
	for _, m := range allMembers {
		if m.Name != "" {
			memberContext = append(memberContext, fmt.Sprintf("%s=%s", m.Name, m.Handle))
		} else {
			memberContext = append(memberContext, m.Handle)
		}
	}

	// Include sender's known name in their context string
	senderContext := senderHandle
	if senderName != "" {
		senderContext = fmt.Sprintf("%s (%s)", senderName, senderHandle)
	}

	// ── Parse the message with Claude ────────────────────────────
	parsed, err := c.Parser.Parse(text, senderContext, memberContext)
	if err != nil {
		log.Printf("error parsing message: %v", err)
		return
	}

	// ── Route by intent ──────────────────────────────────────────
	var reply string
	var effect string

	switch parsed.Intent {
	case parser.IntentAddExpense:
		reply, err = c.handleAddExpense(groupID, parsed)
	case parser.IntentCustomSplit:
		reply, err = c.handleCustomSplit(groupID, parsed)
	case parser.IntentCheckBalance:
		reply, err = c.handleCheckBalance(groupID)
	case parser.IntentSettle:
		reply, err = c.handleSettle(groupID, parsed)
		if err == nil && strings.HasPrefix(reply, "Settled") {
			effect = "confetti"
		}
	case parser.IntentQuery:
		reply, err = c.handleQuery(groupID, parsed)
	case parser.IntentRegister:
		reply, err = c.handleRegister(memberID, parsed)
	case parser.IntentRegisterMember:
		reply, err = c.handleRegisterMember(groupID, parsed)
	case parser.IntentVoidExpense:
		reply, err = c.handleVoidExpense(groupID, parsed)
	case parser.IntentEditExpense:
		reply, err = c.handleEditExpense(groupID, parsed)
	case parser.IntentIgnore:
		return // not an expense-related message
	}

	if err != nil {
		log.Printf("error handling %s: %v", parsed.Intent, err)
		reply = "Something went wrong processing that. Try again?"
		effect = ""
	}

	if reply != "" {
		if err := c.LinqClient.SendText(chatID, reply, effect); err != nil {
			log.Printf("error sending reply: %v", err)
		}
	}

	// React only when the expense was actually logged (not on unknown-member or error replies)
	if (parsed.Intent == parser.IntentAddExpense || parsed.Intent == parser.IntentCustomSplit) &&
		err == nil && strings.HasPrefix(reply, "Got it") {
		_ = c.LinqClient.React(messageID, "like")
	}
	if parsed.Intent == parser.IntentSettle && effect == "confetti" {
		_ = c.LinqClient.React(messageID, "love")
	}
}

// ── Intent handlers ──────────────────────────────────────────────────

// unknownMemberReply returns a friendly message when a handle can't be found.
// Nothing is logged when this is returned.
func unknownMemberReply(handle string) string {
	return fmt.Sprintf(
		"I don't recognise %s — nothing was logged. Register them first with: \"<name> is <phone>\" (e.g. \"Jake is +15551234567\"), then retry.",
		handle,
	)
}

func (c *Config) handleRegisterMember(groupID int64, p *parser.ParsedMessage) (string, error) {
	name := strings.TrimSpace(p.RegisterName)
	handle := strings.TrimSpace(p.RegisterHandle)
	if name == "" || handle == "" {
		return "Couldn't catch that. Try: \"Jake is +15551234567\".", nil
	}
	if _, err := c.Store.EnsureMember(groupID, handle, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("Got it, registered %s (%s)! Now retry your command.", name, handle), nil
}

func (c *Config) handleAddExpense(groupID int64, p *parser.ParsedMessage) (string, error) {
	if p.Amount <= 0 {
		return "Couldn't parse an amount — try again, e.g. \"$47.50 groceries\".", nil
	}

	payerID, err := c.Store.GetMemberByHandle(groupID, p.Payer)
	if errors.Is(err, sql.ErrNoRows) {
		return unknownMemberReply(p.Payer), nil
	}
	if err != nil {
		return "", err
	}

	allMembers, err := c.Store.GetAllMembers(groupID)
	if err != nil {
		return "", err
	}

	if len(allMembers) == 0 {
		return "No members in this group yet.", nil
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

	names := make([]string, len(allMembers))
	for i, m := range allMembers {
		names[i] = displayName(m)
	}

	payerInfo, _ := c.Store.GetMemberInfo(payerID)
	return fmt.Sprintf(
		"Got it! %s paid $%.2f for %s — split evenly between %s ($%.2f each).",
		displayName(payerInfo), p.Amount, p.Description, strings.Join(names, ", "), splitAmt,
	), nil
}

func (c *Config) handleCustomSplit(groupID int64, p *parser.ParsedMessage) (string, error) {
	if p.Amount <= 0 {
		return "Couldn't parse an amount — try again, e.g. \"$60 dinner, exclude @Jake\".", nil
	}

	payerID, err := c.Store.GetMemberByHandle(groupID, p.Payer)
	if errors.Is(err, sql.ErrNoRows) {
		return unknownMemberReply(p.Payer), nil
	}
	if err != nil {
		return "", err
	}

	allMembers, err := c.Store.GetAllMembers(groupID)
	if err != nil {
		return "", err
	}

	nameMap := make(map[int64]string, len(allMembers))
	for _, m := range allMembers {
		nameMap[m.ID] = displayName(m)
	}

	splits := make(map[int64]float64)

	if len(p.CustomSplit) > 0 {
		// Explicit amounts per person
		for handle, amt := range p.CustomSplit {
			memberID, err := c.Store.GetMemberByHandle(groupID, handle)
			if errors.Is(err, sql.ErrNoRows) {
				return unknownMemberReply(handle), nil
			}
			if err != nil {
				return "", err
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
	payerName := displayName(payerInfo)

	if len(p.CustomSplit) > 0 {
		// Show per-person amounts
		parts := make([]string, 0, len(splits))
		for memberID, amt := range splits {
			parts = append(parts, fmt.Sprintf("%s $%.2f", nameMap[memberID], amt))
		}
		sort.Strings(parts)
		return fmt.Sprintf(
			"Got it! %s paid $%.2f for %s — %s.",
			payerName, p.Amount, p.Description, strings.Join(parts, ", "),
		), nil
	}

	// Even split (possibly with exclusions)
	memberNames := make([]string, 0, len(splits))
	for memberID := range splits {
		memberNames = append(memberNames, nameMap[memberID])
	}
	sort.Strings(memberNames)
	splitAmt := p.Amount / float64(len(splits))
	return fmt.Sprintf(
		"Got it! %s paid $%.2f for %s — split evenly between %s ($%.2f each).",
		payerName, p.Amount, p.Description, strings.Join(memberNames, ", "), splitAmt,
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
	if p.Amount <= 0 {
		return "Couldn't parse an amount — try again, e.g. \"Jake paid Mike $30\".", nil
	}

	fromID, err := c.Store.GetMemberByHandle(groupID, p.SettleFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return unknownMemberReply(p.SettleFrom), nil
	}
	if err != nil {
		return "", err
	}

	toID, err := c.Store.GetMemberByHandle(groupID, p.SettleTo)
	if errors.Is(err, sql.ErrNoRows) {
		return unknownMemberReply(p.SettleTo), nil
	}
	if err != nil {
		return "", err
	}

	if fromID == toID {
		return "That's the same person — settlement not recorded.", nil
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

func (c *Config) handleVoidExpense(groupID int64, p *parser.ParsedMessage) (string, error) {
	e, err := c.resolveExpenseRef(groupID, p.ExpenseRef)
	if errors.Is(err, sql.ErrNoRows) {
		return "Couldn't find that expense.", nil
	}
	if err != nil {
		return "", err
	}

	if err := c.Store.VoidExpense(e.ID); err != nil {
		return "", err
	}

	return fmt.Sprintf("Removed: $%.2f for %s.", e.Amount, e.Description), nil
}

func (c *Config) handleEditExpense(groupID int64, p *parser.ParsedMessage) (string, error) {
	e, err := c.resolveExpenseRef(groupID, p.ExpenseRef)
	if errors.Is(err, sql.ErrNoRows) {
		return "Couldn't find that expense.", nil
	}
	if err != nil {
		return "", err
	}

	newAmount := e.Amount
	if p.NewAmount > 0 {
		newAmount = p.NewAmount
	}
	newDescription := e.Description
	if p.NewDescription != "" {
		newDescription = p.NewDescription
	}

	// Recalculate splits proportionally if amount changed
	newSplits := e.Splits
	if p.NewAmount > 0 && e.Amount > 0 {
		ratio := p.NewAmount / e.Amount
		newSplits = make(map[int64]float64, len(e.Splits))
		for memberID, amt := range e.Splits {
			newSplits[memberID] = math.Round(amt*ratio*100) / 100
		}
	}

	if err := c.Store.EditExpense(e.ID, newAmount, newDescription, newSplits); err != nil {
		return "", err
	}

	return fmt.Sprintf("Updated: $%.2f for %s.", newAmount, newDescription), nil
}

// resolveExpenseRef finds an expense by ref string — "last" or empty returns the most recent.
func (c *Config) resolveExpenseRef(groupID int64, ref string) (*db.ExpenseRecord, error) {
	if ref == "" || strings.ToLower(ref) == "last" {
		return c.Store.GetLastExpense(groupID)
	}
	return c.Store.FindExpenseByRef(groupID, ref)
}

func (c *Config) handleRegister(memberID int64, p *parser.ParsedMessage) (string, error) {
	name := strings.TrimSpace(p.RegisterName)
	if name == "" {
		return "Couldn't catch your name — try again, e.g. \"I'm Jake\".", nil
	}

	if err := c.Store.UpdateMemberName(memberID, name); err != nil {
		return "", err
	}

	return fmt.Sprintf("Got it, I'll call you %s!", name), nil
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
