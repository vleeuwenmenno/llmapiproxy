package chatv2

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tempStore creates a temporary Store backed by a temp file database.
func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "chatv2_test.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// --- Session CRUD ---

func TestCreateSession(t *testing.T) {
	s := tempStore(t)

	sess, err := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("expected non-empty UUID")
	}
	if len(sess.ID) != 36 { // UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
		t.Errorf("expected UUID format, got %q", sess.ID)
	}
	if sess.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %q", sess.Model)
	}
	if sess.CreatedAt == 0 || sess.UpdatedAt == 0 {
		t.Error("expected non-zero timestamps")
	}
	if sess.Temperature != 0.7 {
		t.Errorf("expected temperature 0.7, got %f", sess.Temperature)
	}
	if sess.TopP != 1.0 {
		t.Errorf("expected top_p 1.0, got %f", sess.TopP)
	}
	if sess.MaxTokens != 4096 {
		t.Errorf("expected max_tokens 4096, got %d", sess.MaxTokens)
	}
}

func TestCreateSession_Defaults(t *testing.T) {
	s := tempStore(t)

	// Create with empty model and zero values — should get defaults.
	sess, err := s.CreateSession("My Chat", "", "", 0, 0, 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Model != "gpt-4o" {
		t.Errorf("expected default model gpt-4o, got %q", sess.Model)
	}
	if sess.Temperature != 0.7 {
		t.Errorf("expected default temperature 0.7, got %f", sess.Temperature)
	}
	if sess.TopP != 1.0 {
		t.Errorf("expected default top_p 1.0, got %f", sess.TopP)
	}
	if sess.MaxTokens != 4096 {
		t.Errorf("expected default max_tokens 4096, got %d", sess.MaxTokens)
	}
	if sess.Title != "My Chat" {
		t.Errorf("expected title 'My Chat', got %q", sess.Title)
	}
}

func TestGetSession(t *testing.T) {
	s := tempStore(t)

	sess, err := s.CreateSession("Test", "gpt-4o", "be helpful", 0.5, 0.9, 2048)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ID != sess.ID {
		t.Errorf("expected ID %q, got %q", sess.ID, got.ID)
	}
	if got.Title != "Test" {
		t.Errorf("expected title 'Test', got %q", got.Title)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("expected model 'gpt-4o', got %q", got.Model)
	}
	if got.SystemPrompt != "be helpful" {
		t.Errorf("expected system_prompt 'be helpful', got %q", got.SystemPrompt)
	}
	if got.Temperature != 0.5 {
		t.Errorf("expected temperature 0.5, got %f", got.Temperature)
	}
	if got.TopP != 0.9 {
		t.Errorf("expected top_p 0.9, got %f", got.TopP)
	}
	if got.MaxTokens != 2048 {
		t.Errorf("expected max_tokens 2048, got %d", got.MaxTokens)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	s := tempStore(t)

	_, err := s.GetSession("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestUpdateSession(t *testing.T) {
	s := tempStore(t)

	sess, err := s.CreateSession("Original", "gpt-4o", "", 0.7, 1.0, 4096)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Ensure time passes so updated_at is strictly greater.
	time.Sleep(5 * time.Millisecond)

	err = s.UpdateSession(sess.ID, "Updated Title", "claude-3.5-sonnet", "You are a poet", 0.3, 0.8, 8192)
	if err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}

	got, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Title != "Updated Title" {
		t.Errorf("expected title 'Updated Title', got %q", got.Title)
	}
	if got.Model != "claude-3.5-sonnet" {
		t.Errorf("expected model 'claude-3.5-sonnet', got %q", got.Model)
	}
	if got.SystemPrompt != "You are a poet" {
		t.Errorf("expected system_prompt 'You are a poet', got %q", got.SystemPrompt)
	}
	if got.Temperature != 0.3 {
		t.Errorf("expected temperature 0.3, got %f", got.Temperature)
	}
	if got.TopP != 0.8 {
		t.Errorf("expected top_p 0.8, got %f", got.TopP)
	}
	if got.MaxTokens != 8192 {
		t.Errorf("expected max_tokens 8192, got %d", got.MaxTokens)
	}
	if got.UpdatedAt <= sess.UpdatedAt {
		t.Errorf("expected updated_at to advance: got %d, original %d", got.UpdatedAt, sess.UpdatedAt)
	}
}

func TestUpdateSession_NotFound(t *testing.T) {
	s := tempStore(t)

	err := s.UpdateSession("nonexistent", "", "", "", 0, 0, 0)
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestDeleteSession(t *testing.T) {
	s := tempStore(t)

	sess, err := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Add a message to verify CASCADE.
	_, err = s.SaveMessage(sess.ID, "user", "Hello", 0, 0, "", 0, 0)
	if err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	err = s.DeleteSession(sess.ID)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Verify session is gone.
	_, err = s.GetSession(sess.ID)
	if err == nil {
		t.Fatal("expected error getting deleted session")
	}

	// Verify messages are gone (CASCADE).
	msgs, err := s.ListMessages(sess.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages after delete, got %d", len(msgs))
	}
}

func TestDeleteSession_NotFound(t *testing.T) {
	s := tempStore(t)

	err := s.DeleteSession("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestDeleteAllSessions(t *testing.T) {
	s := tempStore(t)

	// Create multiple sessions with messages.
	sess1, _ := s.CreateSession("Chat 1", "gpt-4o", "", 0.7, 1.0, 4096)
	sess2, _ := s.CreateSession("Chat 2", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess1.ID, "user", "Hello 1", 0, 0, "", 0, 0)
	s.SaveMessage(sess2.ID, "user", "Hello 2", 0, 0, "", 0, 0)

	err := s.DeleteAllSessions()
	if err != nil {
		t.Fatalf("DeleteAllSessions: %v", err)
	}

	summaries, err := s.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("expected 0 sessions after delete all, got %d", len(summaries))
	}
}

// --- ListSessions ---

func TestListSessions_Empty(t *testing.T) {
	s := tempStore(t)

	summaries, err := s.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(summaries))
	}
}

func TestListSessions_OrderedByRecentActivity(t *testing.T) {
	s := tempStore(t)

	// Create sessions with a small time gap to ensure deterministic ordering.
	sess1, _ := s.CreateSession("First", "gpt-4o", "", 0.7, 1.0, 4096)
	time.Sleep(10 * time.Millisecond)
	sess2, _ := s.CreateSession("Second", "gpt-4o", "", 0.7, 1.0, 4096)
	time.Sleep(10 * time.Millisecond)
	sess3, _ := s.CreateSession("Third", "gpt-4o", "", 0.7, 1.0, 4096)

	// Add a message to sess1 making it the most recently active.
	time.Sleep(10 * time.Millisecond)
	s.SaveMessage(sess1.ID, "user", "Recent message", 10, 20, "gpt-4o", 500, 20)

	summaries, err := s.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(summaries) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(summaries))
	}

	// sess1 should be first (most recent activity from the message).
	if summaries[0].ID != sess1.ID {
		t.Errorf("expected first session to be %q (most recently active), got %q", sess1.ID, summaries[0].ID)
	}

	// Check that message counts are correct.
	for _, ss := range summaries {
		if ss.ID == sess1.ID {
			if ss.MessageCount != 1 {
				t.Errorf("expected 1 message for sess1, got %d", ss.MessageCount)
			}
			if ss.TotalCompletionTokens != 10 {
				t.Errorf("expected 10 completion tokens, got %d", ss.TotalCompletionTokens)
			}
			if ss.TotalPromptTokens != 20 {
				t.Errorf("expected 20 prompt tokens, got %d", ss.TotalPromptTokens)
			}
		}
		if ss.ID == sess2.ID || ss.ID == sess3.ID {
			if ss.MessageCount != 0 {
				t.Errorf("expected 0 messages for session %s, got %d", ss.ID, ss.MessageCount)
			}
		}
	}
}

// --- Messages ---

func TestSaveMessage(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)

	msg, err := s.SaveMessage(sess.ID, "user", "Hello, world!", 15, 30, "gpt-4o", 1200.5, 12.5)
	if err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if msg.ID == 0 {
		t.Error("expected non-zero autoincrement ID")
	}
	if msg.SessionID != sess.ID {
		t.Errorf("expected session_id %q, got %q", sess.ID, msg.SessionID)
	}
	if msg.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Role)
	}
	if msg.Content != "Hello, world!" {
		t.Errorf("expected content 'Hello, world!', got %q", msg.Content)
	}
	if msg.Tokens != 15 {
		t.Errorf("expected 15 tokens, got %d", msg.Tokens)
	}
	if msg.PromptTokens != 30 {
		t.Errorf("expected 30 prompt_tokens, got %d", msg.PromptTokens)
	}
	if msg.Model != "gpt-4o" {
		t.Errorf("expected model 'gpt-4o', got %q", msg.Model)
	}
	if msg.DurationMs != 1200.5 {
		t.Errorf("expected duration_ms 1200.5, got %f", msg.DurationMs)
	}
	if msg.TPS != 12.5 {
		t.Errorf("expected tps 12.5, got %f", msg.TPS)
	}
	if msg.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}
}

func TestSaveMessage_UpdatesSessionTimestamp(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)
	originalUpdatedAt := sess.UpdatedAt

	time.Sleep(10 * time.Millisecond)
	s.SaveMessage(sess.ID, "user", "Hello", 0, 0, "", 0, 0)

	updated, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if updated.UpdatedAt <= originalUpdatedAt {
		t.Error("expected updated_at to be bumped after SaveMessage")
	}
}

func TestListMessages_ChronologicalOrder(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)

	s.SaveMessage(sess.ID, "user", "First", 0, 0, "", 0, 0)
	time.Sleep(10 * time.Millisecond)
	s.SaveMessage(sess.ID, "assistant", "Second", 0, 0, "", 0, 0)
	time.Sleep(10 * time.Millisecond)
	s.SaveMessage(sess.ID, "user", "Third", 0, 0, "", 0, 0)

	msgs, err := s.ListMessages(sess.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "First" {
		t.Errorf("expected first message 'First', got %q", msgs[0].Content)
	}
	if msgs[1].Content != "Second" {
		t.Errorf("expected second message 'Second', got %q", msgs[1].Content)
	}
	if msgs[2].Content != "Third" {
		t.Errorf("expected third message 'Third', got %q", msgs[2].Content)
	}

	// Verify IDs are in ascending order.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].ID <= msgs[i-1].ID {
			t.Errorf("message IDs not in ascending order: %d <= %d", msgs[i].ID, msgs[i-1].ID)
		}
	}
}

func TestListMessages_Empty(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)

	msgs, err := s.ListMessages(sess.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

// --- SearchSessions ---

func TestSearchSessions_ByTitle(t *testing.T) {
	s := tempStore(t)

	s.CreateSession("Go Programming Tips", "gpt-4o", "", 0.7, 1.0, 4096)
	s.CreateSession("Python Data Science", "gpt-4o", "", 0.7, 1.0, 4096)
	s.CreateSession("Rust Memory Safety", "gpt-4o", "", 0.7, 1.0, 4096)

	results, err := s.SearchSessions("Python")
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Title != "Python Data Science" {
		t.Errorf("expected title 'Python Data Science', got %q", results[0].Title)
	}
}

func TestSearchSessions_ByMessageContent(t *testing.T) {
	s := tempStore(t)

	sess1, _ := s.CreateSession("Chat about fruits", "gpt-4o", "", 0.7, 1.0, 4096)
	sess2, _ := s.CreateSession("Chat about cars", "gpt-4o", "", 0.7, 1.0, 4096)

	s.SaveMessage(sess1.ID, "user", "I love eating strawberries and blueberries", 0, 0, "", 0, 0)
	s.SaveMessage(sess2.ID, "user", "My favorite car is a Toyota Camry", 0, 0, "", 0, 0)

	results, err := s.SearchSessions("strawberries")
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ID != sess1.ID {
		t.Errorf("expected session %q, got %q", sess1.ID, results[0].ID)
	}
}

func TestSearchSessions_MultipleMatches(t *testing.T) {
	s := tempStore(t)

	s.CreateSession("Go Programming", "gpt-4o", "", 0.7, 1.0, 4096)
	sess2, _ := s.CreateSession("Python Programming", "gpt-4o", "", 0.7, 1.0, 4096)
	s.CreateSession("Rust Programming", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess2.ID, "user", "I love programming in Python!", 0, 0, "", 0, 0)

	results, err := s.SearchSessions("Programming")
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}
}

func TestSearchSessions_NoMatch(t *testing.T) {
	s := tempStore(t)

	s.CreateSession("Go Programming", "gpt-4o", "", 0.7, 1.0, 4096)

	results, err := s.SearchSessions("quantum physics")
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestSearchSessions_CaseInsensitive(t *testing.T) {
	s := tempStore(t)

	s.CreateSession("Go Programming Tips", "gpt-4o", "", 0.7, 1.0, 4096)

	results, err := s.SearchSessions("programming")
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result (case insensitive), got %d", len(results))
	}
}

// --- Model Defaults ---

func TestGetModelDefaults_NotSet(t *testing.T) {
	s := tempStore(t)

	md, err := s.GetModelDefaults("gpt-4o")
	if err != nil {
		t.Fatalf("GetModelDefaults: %v", err)
	}
	if md.ModelID != "gpt-4o" {
		t.Errorf("expected model_id 'gpt-4o', got %q", md.ModelID)
	}
	if md.Temperature != 0.7 {
		t.Errorf("expected default temperature 0.7, got %f", md.Temperature)
	}
	if md.TopP != 1.0 {
		t.Errorf("expected default top_p 1.0, got %f", md.TopP)
	}
	if md.MaxTokens != 4096 {
		t.Errorf("expected default max_tokens 4096, got %d", md.MaxTokens)
	}
}

func TestSetModelDefaults(t *testing.T) {
	s := tempStore(t)

	err := s.SetModelDefaults("claude-3.5-sonnet", 0.5, 0.9, 8192, "You are a coding assistant.")
	if err != nil {
		t.Fatalf("SetModelDefaults: %v", err)
	}

	md, err := s.GetModelDefaults("claude-3.5-sonnet")
	if err != nil {
		t.Fatalf("GetModelDefaults: %v", err)
	}
	if md.ModelID != "claude-3.5-sonnet" {
		t.Errorf("expected model_id 'claude-3.5-sonnet', got %q", md.ModelID)
	}
	if md.Temperature != 0.5 {
		t.Errorf("expected temperature 0.5, got %f", md.Temperature)
	}
	if md.TopP != 0.9 {
		t.Errorf("expected top_p 0.9, got %f", md.TopP)
	}
	if md.MaxTokens != 8192 {
		t.Errorf("expected max_tokens 8192, got %d", md.MaxTokens)
	}
	if md.SystemPrompt != "You are a coding assistant." {
		t.Errorf("expected system_prompt 'You are a coding assistant.', got %q", md.SystemPrompt)
	}
}

func TestSetModelDefaults_UpdateExisting(t *testing.T) {
	s := tempStore(t)

	s.SetModelDefaults("gpt-4o", 0.7, 1.0, 4096, "")
	s.SetModelDefaults("gpt-4o", 0.3, 0.8, 2048, "Be concise")

	md, err := s.GetModelDefaults("gpt-4o")
	if err != nil {
		t.Fatalf("GetModelDefaults: %v", err)
	}
	if md.Temperature != 0.3 {
		t.Errorf("expected updated temperature 0.3, got %f", md.Temperature)
	}
	if md.MaxTokens != 2048 {
		t.Errorf("expected updated max_tokens 2048, got %d", md.MaxTokens)
	}
	if md.SystemPrompt != "Be concise" {
		t.Errorf("expected updated system_prompt 'Be concise', got %q", md.SystemPrompt)
	}
}

func TestSetModelDefaults_MultipleModels(t *testing.T) {
	s := tempStore(t)

	s.SetModelDefaults("gpt-4o", 0.7, 1.0, 4096, "GPT defaults")
	s.SetModelDefaults("claude-3.5-sonnet", 0.5, 0.9, 8192, "Claude defaults")

	md1, _ := s.GetModelDefaults("gpt-4o")
	md2, _ := s.GetModelDefaults("claude-3.5-sonnet")

	if md1.Temperature != 0.7 {
		t.Errorf("expected gpt-4o temperature 0.7, got %f", md1.Temperature)
	}
	if md2.Temperature != 0.5 {
		t.Errorf("expected claude temperature 0.5, got %f", md2.Temperature)
	}
	if md1.SystemPrompt != "GPT defaults" {
		t.Errorf("expected gpt-4o system_prompt 'GPT defaults', got %q", md1.SystemPrompt)
	}
	if md2.SystemPrompt != "Claude defaults" {
		t.Errorf("expected claude system_prompt 'Claude defaults', got %q", md2.SystemPrompt)
	}
}

// --- Export ---

func TestExportSessionMarkdown(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("My Test Chat", "gpt-4o", "You are helpful", 0.7, 1.0, 4096)
	s.SaveMessage(sess.ID, "user", "Hello!", 0, 0, "", 0, 0)
	s.SaveMessage(sess.ID, "assistant", "Hi there! How can I help?", 10, 20, "gpt-4o", 500, 20)

	md, err := s.ExportSessionMarkdown(sess.ID)
	if err != nil {
		t.Fatalf("ExportSessionMarkdown: %v", err)
	}

	if !strings.Contains(md, "# My Test Chat") {
		t.Errorf("expected markdown to contain '# My Test Chat', got %q", md)
	}
	if !strings.Contains(md, "**You:** Hello!") {
		t.Errorf("expected markdown to contain '**You:** Hello!', got %q", md)
	}
	if !strings.Contains(md, "**Assistant:** Hi there! How can I help?") {
		t.Errorf("expected markdown to contain '**Assistant:** Hi there! How can I help?', got %q", md)
	}
	if !strings.Contains(md, "## System Prompt") {
		t.Errorf("expected markdown to contain '## System Prompt', got %q", md)
	}
	if !strings.Contains(md, "You are helpful") {
		t.Errorf("expected markdown to contain system prompt, got %q", md)
	}
	if !strings.Contains(md, "Model: gpt-4o") {
		t.Errorf("expected markdown to contain model info, got %q", md)
	}
}

func TestExportSessionMarkdown_NoSystemPrompt(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("No System Prompt", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess.ID, "user", "Hello!", 0, 0, "", 0, 0)

	md, err := s.ExportSessionMarkdown(sess.ID)
	if err != nil {
		t.Fatalf("ExportSessionMarkdown: %v", err)
	}
	if strings.Contains(md, "## System Prompt") {
		t.Errorf("expected no System Prompt section when system_prompt is empty, got %q", md)
	}
}

func TestExportSessionMarkdown_NotFound(t *testing.T) {
	s := tempStore(t)

	_, err := s.ExportSessionMarkdown("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestExportSessionJSON(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("JSON Export Test", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess.ID, "user", "What is 2+2?", 0, 0, "", 0, 0)
	s.SaveMessage(sess.ID, "assistant", "The answer is 4.", 5, 10, "gpt-4o", 200, 25)

	jsonStr, err := s.ExportSessionJSON(sess.ID)
	if err != nil {
		t.Fatalf("ExportSessionJSON: %v", err)
	}

	// Validate JSON is parseable.
	var msgs []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &msgs); err != nil {
		t.Fatalf("invalid JSON: %v\njson: %s", err, jsonStr)
	}

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages in JSON, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Errorf("expected first message role 'user', got %v", msgs[0]["role"])
	}
	if msgs[0]["content"] != "What is 2+2?" {
		t.Errorf("expected first message content 'What is 2+2?', got %v", msgs[0]["content"])
	}
	if msgs[1]["role"] != "assistant" {
		t.Errorf("expected second message role 'assistant', got %v", msgs[1]["role"])
	}
	if msgs[1]["content"] != "The answer is 4." {
		t.Errorf("expected second message content 'The answer is 4.', got %v", msgs[1]["content"])
	}
}

func TestExportSessionJSON_EmptySession(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("Empty", "gpt-4o", "", 0.7, 1.0, 4096)

	jsonStr, err := s.ExportSessionJSON(sess.ID)
	if err != nil {
		t.Fatalf("ExportSessionJSON: %v", err)
	}

	var msgs []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &msgs); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

// --- GenerateTitle ---

func TestGenerateTitle(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess.ID, "user", "How do I write a Go web server?", 0, 0, "", 0, 0)

	title, err := s.GenerateTitle(sess.ID)
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	if title != "How do I write a Go web server?" {
		t.Errorf("expected title 'How do I write a Go web server?', got %q", title)
	}

	// Verify the session title was updated.
	got, _ := s.GetSession(sess.ID)
	if got.Title != title {
		t.Errorf("session title not updated: expected %q, got %q", title, got.Title)
	}
}

func TestGenerateTitle_Truncation(t *testing.T) {
	s := tempStore(t)

	longMsg := "This is a very long message that should be truncated because it exceeds the eighty character limit that we have set for session titles in our chat application"
	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess.ID, "user", longMsg, 0, 0, "", 0, 0)

	title, err := s.GenerateTitle(sess.ID)
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	runes := []rune(title)
	// Allow for the ellipsis character.
	if len(runes) > 81 { // 80 chars + ellipsis
		t.Errorf("expected title <= 81 runes, got %d: %q", len(runes), title)
	}
	if !strings.HasSuffix(title, "…") {
		t.Errorf("expected truncated title to end with ellipsis, got %q", title)
	}
}

func TestGenerateTitle_Multiline(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess.ID, "user", "First line of message\nSecond line of message", 0, 0, "", 0, 0)

	title, err := s.GenerateTitle(sess.ID)
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	if title != "First line of message" {
		t.Errorf("expected first line only, got %q", title)
	}
}

func TestGenerateTitle_NoUserMessage(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess.ID, "assistant", "Hello!", 0, 0, "", 0, 0)

	_, err := s.GenerateTitle(sess.ID)
	if err == nil {
		t.Fatal("expected error when no user message exists")
	}
}

func TestGenerateTitle_EmptyUserMessage(t *testing.T) {
	s := tempStore(t)

	sess, _ := s.CreateSession("", "gpt-4o", "", 0.7, 1.0, 4096)
	s.SaveMessage(sess.ID, "user", "", 0, 0, "", 0, 0)
	s.SaveMessage(sess.ID, "assistant", "Sure!", 0, 0, "", 0, 0)

	_, err := s.GenerateTitle(sess.ID)
	if err == nil {
		t.Fatal("expected error when user message is empty")
	}
}

// --- Database ---

func TestOpenStore_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "chatv2.db")

	// OpenStore should NOT create intermediate directories — that's the caller's job.
	// Let's verify it fails gracefully if the directory doesn't exist.
	_, err := OpenStore(path)
	if err == nil {
		t.Fatal("expected error when parent directory doesn't exist")
	}
}

func TestOpenStore_WALMode(t *testing.T) {
	s := tempStore(t)

	// Verify WAL mode is active.
	var journalMode string
	err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("expected journal_mode 'wal', got %q", journalMode)
	}
}

func TestOpenStore_ForeignKeysEnabled(t *testing.T) {
	s := tempStore(t)

	var fkEnabled int
	err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fkEnabled)
	if err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if fkEnabled != 1 {
		t.Errorf("expected foreign_keys=1, got %d", fkEnabled)
	}
}

// --- Integration / Lifecycle ---

func TestSessionCRUD_Lifecycle(t *testing.T) {
	s := tempStore(t)

	// Create.
	sess, err := s.CreateSession("Lifecycle Test", "gpt-4o", "be helpful", 0.5, 0.9, 2048)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Read.
	got, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Title != "Lifecycle Test" {
		t.Errorf("expected title 'Lifecycle Test', got %q", got.Title)
	}

	// Update.
	err = s.UpdateSession(sess.ID, "Updated Lifecycle", "claude-3.5-sonnet", "be concise", 0.3, 0.8, 1024)
	if err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	got, err = s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession after update: %v", err)
	}
	if got.Title != "Updated Lifecycle" {
		t.Errorf("expected title 'Updated Lifecycle', got %q", got.Title)
	}
	if got.Model != "claude-3.5-sonnet" {
		t.Errorf("expected model 'claude-3.5-sonnet', got %q", got.Model)
	}

	// Delete.
	err = s.DeleteSession(sess.ID)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	_, err = s.GetSession(sess.ID)
	if err == nil {
		t.Fatal("expected error getting deleted session")
	}
}

func TestFullConversation(t *testing.T) {
	s := tempStore(t)

	// Create session.
	sess, _ := s.CreateSession("Full Conversation", "gpt-4o", "", 0.7, 1.0, 4096)

	// Add user message.
	s.SaveMessage(sess.ID, "user", "What is Go?", 0, 0, "", 0, 0)

	// Add assistant message with metadata.
	s.SaveMessage(sess.ID, "assistant", "Go is a programming language.", 50, 100, "gpt-4o", 1500.0, 33.3)

	// List messages.
	msgs, err := s.ListMessages(sess.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("expected first message role 'user', got %q", msgs[0].Role)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("expected second message role 'assistant', got %q", msgs[1].Role)
	}
	if msgs[1].Tokens != 50 {
		t.Errorf("expected 50 tokens, got %d", msgs[1].Tokens)
	}
	if msgs[1].TPS != 33.3 {
		t.Errorf("expected tps 33.3, got %f", msgs[1].TPS)
	}

	// Generate title.
	title, err := s.GenerateTitle(sess.ID)
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	if title != "What is Go?" {
		t.Errorf("expected title 'What is Go?', got %q", title)
	}

	// Export as markdown.
	md, err := s.ExportSessionMarkdown(sess.ID)
	if err != nil {
		t.Fatalf("ExportSessionMarkdown: %v", err)
	}
	if !strings.Contains(md, "What is Go?") {
		t.Errorf("markdown missing user message")
	}
	if !strings.Contains(md, "Go is a programming language.") {
		t.Errorf("markdown missing assistant message")
	}

	// Export as JSON.
	jsonStr, err := s.ExportSessionJSON(sess.ID)
	if err != nil {
		t.Fatalf("ExportSessionJSON: %v", err)
	}
	var jsonMsgs []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &jsonMsgs); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(jsonMsgs) != 2 {
		t.Errorf("expected 2 messages in JSON, got %d", len(jsonMsgs))
	}

	// Verify session list includes this session.
	summaries, err := s.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, ss := range summaries {
		if ss.ID == sess.ID {
			found = true
			if ss.MessageCount != 2 {
				t.Errorf("expected 2 messages in summary, got %d", ss.MessageCount)
			}
		}
	}
	if !found {
		t.Error("session not found in list")
	}
}

// Verify that the data directory works.
func TestOpenStore_DataDir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dataDir, "chatv2.db")

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Quick smoke test — create a session.
	sess, err := store.CreateSession("Data Dir Test", "gpt-4o", "", 0.7, 1.0, 4096)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID == "" {
		t.Error("expected non-empty session ID")
	}

	// Verify the file exists.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("database file not created at %s", path)
	}
}
