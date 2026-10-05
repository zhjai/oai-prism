package account

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestChatProgressMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE chat_messages (
		id TEXT PRIMARY KEY, session_id TEXT, role TEXT, content TEXT,
		reasoning TEXT, status TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO chat_messages(id,session_id,role,content,reasoning,status)
		VALUES('old','session','assistant','old answer','','success')`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	store := openConfigTestStore(t, path)
	rows, err := store.ListChatMessages("session")
	if err != nil || len(rows) != 1 || rows[0].Content != "old answer" || rows[0].Progress != "" {
		t.Fatalf("legacy message migration: %+v, %v", rows, err)
	}
	if err := store.SaveChatMessage(ChatMessageRecord{ID: "new", SessionID: "session", Role: "assistant", Content: "final answer", Progress: "real progress", Reasoning: "summary", Status: "loading"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChatMessage(ChatMessageRecord{ID: "new", SessionID: "session", Role: "assistant", Content: "final answer", Progress: "real progress\nnext", Reasoning: "summary", Status: "success"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	store = openConfigTestStore(t, path)
	rows, err = store.ListChatMessages("session")
	if err != nil || len(rows) != 2 {
		t.Fatalf("message reload: count=%d, error=%v", len(rows), err)
	}
	for _, row := range rows {
		if row.ID == "new" && (row.Content != "final answer" || row.Progress != "real progress\nnext" || row.Reasoning != "summary" || row.Status != "success") {
			t.Fatalf("progress not kept separate: %+v", row)
		}
	}
}
